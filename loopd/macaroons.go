package loopd

import (
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
		"/looprpc.SwapClient/SuggestSwaps": {{
			Entity: "suggestions",
			Action: "read",
		}},
		"/looprpc.SwapClient/GetLiquidityParams": {{
			Entity: "suggestions",
			Action: "read",
		}},
		"/looprpc.SwapClient/SetLiquidityParams": {{
			Entity: "suggestions",
			Action: "write",
		}},
		"/looprpc.SwapClient/Probe": {{
			Entity: "swap",
			Action: "execute",
		}, {
			Entity: "loop",
			Action: "in",
		}},
	}

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
