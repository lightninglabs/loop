package loopd

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"google.golang.org/grpc"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

const (
	// loopMacaroonLocation is the value we use for the loopd macaroons'
	// "Location" field when baking them.
	loopMacaroonLocation = "loop"
)

var (
	// RequiredPermissions is a map of all loop RPC methods and their
	// required macaroon permissions to access loopd.
	RequiredPermissions = map[string][]bakery.Op{
		"/looprpc.SwapClient/LoopOut": {{
			Entity: "swap",
			Action: "execute",
		}, {
			Entity: "loop",
			Action: "out",
		}},
		"/looprpc.SwapClient/LoopIn": {{
			Entity: "swap",
			Action: "execute",
		}, {
			Entity: "loop",
			Action: "in",
		}},
		"/looprpc.SwapClient/Monitor": {{
			Entity: "swap",
			Action: "read",
		}},
		"/looprpc.SwapClient/ListSwaps": {{
			Entity: "swap",
			Action: "read",
		}},
		"/looprpc.SwapClient/SwapInfo": {{
			Entity: "swap",
			Action: "read",
		}},
		"/looprpc.SwapClient/LoopOutTerms": {{
			Entity: "terms",
			Action: "read",
		}, {
			Entity: "loop",
			Action: "out",
		}},
		"/looprpc.SwapClient/LoopOutQuote": {{
			Entity: "swap",
			Action: "read",
		}, {
			Entity: "loop",
			Action: "out",
		}},
		"/looprpc.SwapClient/GetLoopInTerms": {{
			Entity: "terms",
			Action: "read",
		}, {
			Entity: "loop",
			Action: "in",
		}},
		"/looprpc.SwapClient/GetLoopInQuote": {{
			Entity: "swap",
			Action: "read",
		}, {
			Entity: "loop",
			Action: "in",
		}},
		"/looprpc.SwapClient/GetLsatTokens": {{
			Entity: "auth",
			Action: "read",
		}},
	}

	// allPermissions is the list of all existing permissions that exist
	// for loopd's RPC. The default macaroon that is created on startup
	// contains all these permissions and is therefore equivalent to lnd's
	// admin.macaroon but for loop.
	allPermissions = []bakery.Op{{
		Entity: "loop",
		Action: "out",
	}, {
		Entity: "loop",
		Action: "in",
	}, {
		Entity: "swap",
		Action: "execute",
	}, {
		Entity: "swap",
		Action: "read",
	}, {
		Entity: "terms",
		Action: "read",
	}, {
		Entity: "auth",
		Action: "read",
	}}

	// macDbDefaultPw is the default encryption password used to encrypt the
	// loop macaroon database. The macaroon service requires us to set a
	// non-nil password so we set it to an empty string. This will cause the
	// keys to be encrypted on disk but won't provide any security at all as
	// the password is known to anyone.
	//
	// TODO(guggero): Allow the password to be specified by the user. Needs
	// create/unlock calls in the RPC. Using a password should be optional
	// though.
	macDbDefaultPw = []byte("")
)

// startMacaroonService starts the macaroon validation service, creates or
// unlocks the macaroon database and creates the default macaroon if it doesn't
// exist yet. If macaroons are disabled in general in the configuration, none of
// these actions are taken.
func (d *Daemon) startMacaroonService() error {
	// Create the macaroon authentication/authorization service.
	var err error
	d.macaroonService, err = macaroons.NewService(
		d.cfg.DataDir, loopMacaroonLocation, macaroons.IPLockChecker,
	)
	if err != nil {
		return fmt.Errorf("unable to set up macaroon authentication: "+
			"%v", err)
	}

	// Try to unlock the macaroon store with the private password.
	err = d.macaroonService.CreateUnlock(&macDbDefaultPw)
	if err != nil {
		return fmt.Errorf("unable to unlock macaroon DB: %v", err)
	}

	// Create macaroon files for loop CLI to use if they don't exist.
	if !lnrpc.FileExists(d.cfg.MacaroonPath) {
		ctx := context.Background()

		// We only generate one default macaroon that contains all
		// existing permissions (equivalent to the admin.macaroon in
		// lnd). Custom macaroons can be created through the bakery
		// RPC.
		loopMac, err := d.macaroonService.Oven.NewMacaroon(
			ctx, bakery.LatestVersion, nil, allPermissions...,
		)
		if err != nil {
			return err
		}
		loopMacBytes, err := loopMac.M().MarshalBinary()
		if err != nil {
			return err
		}
		err = ioutil.WriteFile(d.cfg.MacaroonPath, loopMacBytes, 0644)
		if err != nil {
			if err := os.Remove(d.cfg.MacaroonPath); err != nil {
				log.Errorf("Unable to remove %s: %v",
					d.cfg.MacaroonPath, err)
			}
			return err
		}
	}

	return nil
}

// stopMacaroonService closes the macaroon database.
func (d *Daemon) stopMacaroonService() error {
	return d.macaroonService.Close()
}

// macaroonInterceptor creates gRPC server options with the macaroon security
// interceptors.
func (d *Daemon) macaroonInterceptor() []grpc.ServerOption {
	unaryInterceptor := d.macaroonService.UnaryServerInterceptor(
		RequiredPermissions,
	)
	streamInterceptor := d.macaroonService.StreamServerInterceptor(
		RequiredPermissions,
	)
	return []grpc.ServerOption{
		grpc.UnaryInterceptor(unaryInterceptor),
		grpc.StreamInterceptor(streamInterceptor),
	}
}
