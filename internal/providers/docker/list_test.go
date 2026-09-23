package docker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/colony-2/pulse/pkg/compute"
)

func TestListActiveContainersWithoutChangingAccounting(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/containers/json" || r.URL.Query().Get("all") != "1" {
			t.Error(r.Method, r.URL)
		}
		rows := []any{}
		for _, state := range []string{"created", "running", "paused", "exited", "dead"} {
			rows = append(rows, map[string]any{"Id": state, "State": state, "Labels": map[string]string{"pulse_managed_by": "pulse", "pulse_launch_id": state, "secret": "hidden"}})
		}
		json.NewEncoder(w).Encode(rows)
	}))
	defer server.Close()
	uncertain := map[string]charge{"pending": {CPU: 1000, Memory: 1024, Slots: 1}}
	p := &Provider{e: &engine{base: server.URL, client: server.Client()}, uncertain: uncertain}
	q := compute.ListRequest{PageSize: 2}
	first, err := p.List(context.Background(), q)
	if err != nil || len(first.Items) != 2 || first.NextPageToken == "" {
		t.Fatal(first, err)
	}
	q.PageToken = first.NextPageToken
	last, err := p.List(context.Background(), q)
	if err != nil || len(last.Items) != 1 || last.Items[0].State != "running" || last.NextPageToken != "" {
		t.Fatal(last, err)
	}
	for _, row := range first.Items {
		if row.Metadata["secret"] != "" {
			t.Fatal("metadata leaked")
		}
	}
	if !reflect.DeepEqual(p.uncertain, map[string]charge{"pending": {CPU: 1000, Memory: 1024, Slots: 1}}) {
		t.Fatal("listing mutated admission")
	}
}
