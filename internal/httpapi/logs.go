package httpapi

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	provider "github.com/colony-2/pulse-provider-api"
	sdk "github.com/colony-2/pulse-provider-api/plugin"
	"github.com/colony-2/pulse/internal/launchlogs"
	"github.com/colony-2/pulse/internal/providerutil"
)

const launchLogPath = "/providers/{provider}/launches/{launch_id}/logs"

const (
	providerCheckpoint = "p."
	fileCheckpoint     = "f."
)

func logQuery(r *http.Request) (provider.LaunchLogRequest, string, error) {
	q := provider.LaunchLogRequest{LaunchID: r.PathValue("launch_id")}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return q, "", provider.ErrLaunchLogInvalidRequest
	}
	var source string
	for key, values := range query {
		if len(values) != 1 {
			return q, "", provider.ErrLaunchLogInvalidRequest
		}
		switch key {
		case "page_size":
			q.PageSize, err = strconv.Atoi(values[0])
			if err != nil || q.PageSize < 1 {
				return q, "", provider.ErrLaunchLogInvalidRequest
			}
		case "page_token":
			value := values[0]
			if len(value) <= 2 || len(value) > 2+base64.RawURLEncoding.EncodedLen(providerutil.MaxLogPageToken) {
				return q, "", provider.ErrLaunchLogCursor
			}
			source = value[:2]
			if source != providerCheckpoint && source != fileCheckpoint {
				return q, "", provider.ErrLaunchLogCursor
			}
			raw, err := base64.RawURLEncoding.Strict().DecodeString(value[2:])
			if err != nil || len(raw) == 0 {
				return q, "", provider.ErrLaunchLogCursor
			}
			q.PageToken = string(raw)
		default:
			return q, "", provider.ErrLaunchLogInvalidRequest
		}
	}
	q, err = providerutil.NormalizeLogRequest(q)
	return q, source, err
}

func (a *API) launchLog(w http.ResponseWriter, r *http.Request) {
	service := r.PathValue("provider")
	p, ok := a.Controller.ProviderSnapshot()[service]
	if !ok {
		write(w, http.StatusNotFound, map[string]string{"error": "unknown provider"})
		return
	}
	q, source, err := logQuery(r)
	if err != nil {
		writeLogError(w, err)
		return
	}
	if !a.enter(w) {
		return
	}
	defer func() { <-a.slots }()
	ctx, cancel := context.WithTimeout(r.Context(), a.Controller.Config.Call)
	defer cancel()
	page, nextSource, err := a.readLog(ctx, service, p, q, source)
	if err == nil {
		err = providerutil.ValidateLogPage(q, page)
	}
	if err != nil {
		writeLogError(w, err)
		return
	}
	token := nextSource + base64.RawURLEncoding.EncodeToString([]byte(page.NextPageToken))
	write(w, http.StatusOK, launchlogs.NewPage(page, token))
}

func (a *API) readLog(ctx context.Context, service string, p provider.Provider, q provider.LaunchLogRequest, source string) (provider.LaunchLogResponse, string, error) {
	store := a.Controller.Scheduler.Logs
	if source == fileCheckpoint {
		if store == nil {
			return provider.LaunchLogResponse{}, "", provider.ErrLaunchLogNotFound
		}
		page, err := store.Read(ctx, service, q)
		return page, fileCheckpoint, err
	}
	nativeErr := provider.ErrLaunchLogUnsupported
	if logs, ok := p.(provider.ProviderWithLaunchLogs); ok {
		page, err := logs.GetLaunchLog(ctx, q)
		if err == nil {
			return page, providerCheckpoint, nil
		}
		nativeErr = err
	}
	if source == "" && store != nil && (errors.Is(nativeErr, provider.ErrLaunchLogUnsupported) ||
		errors.Is(nativeErr, provider.ErrLaunchLogNotFound) || errors.Is(nativeErr, sdk.ErrUnavailable) ||
		errors.Is(nativeErr, sdk.ErrTransport)) {
		page, err := store.Read(ctx, service, q)
		if !errors.Is(err, provider.ErrLaunchLogNotFound) || errors.Is(nativeErr, provider.ErrLaunchLogUnsupported) {
			return page, fileCheckpoint, err
		}
	}
	return provider.LaunchLogResponse{}, "", nativeErr
}

func writeLogError(w http.ResponseWriter, err error) {
	code, message := http.StatusBadGateway, "launch log read failed"
	switch {
	case errors.Is(err, provider.ErrLaunchLogInvalidRequest):
		code, message = http.StatusBadRequest, "invalid launch log request"
	case errors.Is(err, provider.ErrLaunchLogCursor):
		code, message = http.StatusBadRequest, "invalid launch log cursor"
	case errors.Is(err, provider.ErrLaunchLogUnsupported):
		code, message = http.StatusNotImplemented, "provider does not expose retained launch logs"
	case errors.Is(err, provider.ErrLaunchLogNotFound):
		code, message = http.StatusNotFound, "launch log not found"
	case errors.Is(err, sdk.ErrUnavailable):
		code, message = http.StatusServiceUnavailable, "provider is unavailable"
	case errors.Is(err, sdk.ErrSaturated):
		code, message = http.StatusTooManyRequests, "too many provider requests"
	case errors.Is(err, context.DeadlineExceeded):
		code, message = http.StatusGatewayTimeout, "launch log read timed out"
	}
	write(w, code, map[string]string{"error": message})
}
