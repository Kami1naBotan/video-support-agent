package config

import (
	"fmt"
	"net/netip"
	"strings"
)

func ValidateTrustedProxies(values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}

	result := make([]string, 0, len(values))

	for _, raw := range values {
		value := strings.TrimSpace(raw)

		if value == "" {
			return nil, fmt.Errorf("TRUSTED_PROXIES contains an empty entry")
		}

		if address, err := netip.ParseAddr(value); err == nil {
			if address.Zone() != "" || address.IsUnspecified() || address.IsMulticast() {
				return nil, fmt.Errorf("TRUSTED_PROXIES contains an invalid address")
			}

			result = append(result, address.String())
			continue
		}

		prefix, err := netip.ParsePrefix(value)
		if err != nil || prefix.Bits() == 0 {
			return nil, fmt.Errorf("TRUSTED_PROXIES contains an invalid or unrestricted network")
		}

		result = append(result, prefix.Masked().String())
	}

	return result, nil
}
