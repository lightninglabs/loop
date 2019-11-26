package lsat

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	// CondServices is the condition used for a services caveat.
	CondServices = "services"

	// CondCapabilitiesSuffix is the condition suffix used for a service's
	// capabilities caveat. For example, the condition of a capabilities
	// caveat for a service named `loop` would be `loop_capabilities`.
	CondCapabilitiesSuffix = "_capabilities"
)

var (
	// ErrNoServices is an error returned when we attempt to decode the
	// services included in a caveat.
	ErrNoServices = errors.New("no services found")

	// ErrInvalidService is an error returned when we attempt to decode a
	// service with an invalid format.
	ErrInvalidService = errors.New("service must be of the form " +
		"\"name:tier\"")
)

// ServiceTier represents the different possible tiers of an LSAT-enabled
// service.
type ServiceTier uint8

const (
	// BaseTier is the base tier of an LSAT-enabled service. This tier
	// should be used for any new LSATs that are not part of a service tier
	// upgrade.
	BaseTier ServiceTier = iota
)

// Service contains the details of an LSAT-enabled service.
type Service struct {
	// Name is the name of the LSAT-enabled service.
	Name string

	// Tier is the tier of the LSAT-enabled service.
	Tier ServiceTier
}

// NewServicesCaveat creates a new services caveat with the provided caveats.
func NewServicesCaveat(services ...Service) (Caveat, error) {
	value, err := encodeServicesCaveatValue(services...)
	if err != nil {
		return Caveat{}, err
	}
	return Caveat{
		Condition: CondServices,
		Value:     value,
	}, nil
}

// encodeServicesCaveatValue encodes a list of services into the expected format
// of a services caveat's value.
func encodeServicesCaveatValue(services ...Service) (string, error) {
	if len(services) == 0 {
		return "", ErrNoServices
	}

	var s strings.Builder
	for i, service := range services {
		if service.Name == "" {
			return "", errors.New("missing service name")
		}

		fmtStr := "%v:%v"
		if i < len(services)-1 {
			fmtStr += ","
		}

		fmt.Fprintf(&s, fmtStr, service.Name, uint8(service.Tier))
	}

	return s.String(), nil
}

// decodeServicesCaveatValue decodes a list of services from the expected format
// of a services caveat's value.
func decodeServicesCaveatValue(s string) ([]Service, error) {
	if s == "" {
		return nil, ErrNoServices
	}

	rawServices := strings.Split(s, ",")
	services := make([]Service, 0, len(rawServices))
	for _, rawService := range rawServices {
		serviceInfo := strings.Split(rawService, ":")
		if len(serviceInfo) != 2 {
			return nil, ErrInvalidService
		}

		name, tierStr := serviceInfo[0], serviceInfo[1]
		if name == "" {
			return nil, fmt.Errorf("%w: %v", ErrInvalidService,
				"empty name")
		}
		tier, err := strconv.Atoi(tierStr)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidService, err)
		}

		services = append(services, Service{
			Name: name,
			Tier: ServiceTier(tier),
		})
	}

	return services, nil
}

// NewCapabilitiesCaveat creates a new capabilities caveat for the given
// service.
func NewCapabilitiesCaveat(serviceName string, capabilities string) Caveat {
	return Caveat{
		Condition: serviceName + CondCapabilitiesSuffix,
		Value:     capabilities,
	}
}
