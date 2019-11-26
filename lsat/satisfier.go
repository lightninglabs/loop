package lsat

import (
	"fmt"
	"strings"
)

// Satisfier provides a generic interface to satisfy a caveat based on its
// condition.
type Satisfier struct {
	// Condition is the condition of the caveat we'll attempt to satisfy.
	Condition string

	// SatisfyPrevious ensures a caveat is in accordance with a previous one
	// with the same condition. This is needed since caveats of the same
	// condition can be used multiple times as long as they enforce more
	// permissions than the previous.
	//
	// For example, we have a caveat that only allows us to use an LSAT for
	// 7 more days. We can add another caveat that only allows for 3 more
	// days of use and lend it to another party.
	SatisfyPrevious func(previous Caveat, current Caveat) error

	// SatisfyFinal satisfies the final caveat of an LSAT. If multiple
	// caveats with the same condition exist, this will only be executed
	// once all previous caveats are also satisfied.
	SatisfyFinal func(Caveat) error
}

// NewServicesSatisfier implements a satisfier to determine whether the target
// service is authorized for a given LSAT.
//
// TODO(wilmer): Add tier verification?
func NewServicesSatisfier(targetService string) Satisfier {
	return Satisfier{
		Condition: CondServices,
		SatisfyPrevious: func(prev, cur Caveat) error {
			// Construct a set of the services we were previously
			// allowed to access.
			prevServices, err := decodeServicesCaveatValue(prev.Value)
			if err != nil {
				return err
			}
			prevAllowed := make(map[string]struct{}, len(prevServices))
			for _, service := range prevServices {
				prevAllowed[service.Name] = struct{}{}
			}

			// The caveat should not include any new services that
			// weren't previously allowed.
			currentServices, err := decodeServicesCaveatValue(cur.Value)
			if err != nil {
				return err
			}
			for _, service := range currentServices {
				if _, ok := prevAllowed[service.Name]; !ok {
					return fmt.Errorf("service %v not "+
						"previously allowed", service)
				}
			}

			return nil
		},
		SatisfyFinal: func(c Caveat) error {
			services, err := decodeServicesCaveatValue(c.Value)
			if err != nil {
				return err
			}
			for _, service := range services {
				if service.Name == targetService {
					return nil
				}
			}
			return fmt.Errorf("target service %v not authorized",
				targetService)
		},
	}
}

// NewCapabilitiesSatisfier implements a satisfier to determine whether the
// target capability for a service is authorized for a given LSAT.
func NewCapabilitiesSatisfier(service string, targetCapability string) Satisfier {
	return Satisfier{
		Condition: service + CondCapabilitiesSuffix,
		SatisfyPrevious: func(prev, cur Caveat) error {
			// Construct a set of the service's capabilities we were
			// previously allowed to access.
			prevCapabilities := strings.Split(prev.Value, ",")
			allowed := make(map[string]struct{}, len(prevCapabilities))
			for _, capability := range prevCapabilities {
				allowed[capability] = struct{}{}
			}

			// The caveat should not include any new service
			// capabilities that weren't previously allowed.
			currentCapabilities := strings.Split(cur.Value, ",")
			for _, capability := range currentCapabilities {
				if _, ok := allowed[capability]; !ok {
					return fmt.Errorf("capability %v not "+
						"previously allowed", capability)
				}
			}

			return nil
		},
		SatisfyFinal: func(c Caveat) error {
			capabilities := strings.Split(c.Value, ",")
			for _, capability := range capabilities {
				if capability == targetCapability {
					return nil
				}
			}
			return fmt.Errorf("target capability %v not authorized",
				targetCapability)
		},
	}
}
