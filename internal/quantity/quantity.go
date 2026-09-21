// Package quantity parses the public c2j quantity vocabulary without importing c2j.
package quantity

import (
	"fmt"
	"github.com/colony-2/cortex/pkg/compute"
	"math/big"
	"regexp"
	"strings"
)

var cpuPattern = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)(m?)$`)
var bytesPattern = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)([kMGTPE]|[KMGTPE]i)$`)

func Parse(value string, cpu bool) (int64, error) {
	if len(value) > 128 {
		return 0, fmt.Errorf("quantity too long")
	}
	pattern := bytesPattern
	if cpu {
		pattern = cpuPattern
	}
	parts := pattern.FindStringSubmatch(value)
	if parts == nil {
		return 0, fmt.Errorf("invalid quantity %q", value)
	}
	n, ok := new(big.Rat).SetString(parts[1])
	if !ok || n.Sign() <= 0 {
		return 0, fmt.Errorf("quantity must be positive")
	}
	mult := big.NewInt(1)
	if cpu {
		if parts[2] != "m" {
			mult.SetInt64(1000)
		}
	} else {
		suffix := parts[2]
		power := strings.Index("kMGTPE", strings.ReplaceAll(suffix[:1], "K", "k")) + 1
		base := int64(1000)
		if strings.HasSuffix(suffix, "i") {
			base = 1024
		}
		mult.Exp(big.NewInt(base), big.NewInt(int64(power)), nil)
	}
	n.Mul(n, new(big.Rat).SetInt(mult))
	if !n.IsInt() || !n.Num().IsInt64() || n.Num().Int64() > compute.MaxQuantity {
		return 0, fmt.Errorf("quantity is fractional or too large")
	}
	return n.Num().Int64(), nil
}
func CPU(n int64) string { return fmt.Sprintf("%dm", n) }
func Bytes(n int64) string {
	if n%1024 == 0 {
		return fmt.Sprintf("%dKi", n/1024)
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%d.%03d", n/1000, n%1000), "0"), ".") + "k"
}
