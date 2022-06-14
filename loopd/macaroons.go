package loopd

const (
	// loopMacaroonLocation is the value we use for the loopd macaroons'
	// "Location" field when baking them.
	loopMacaroonLocation = "loop"
)

var (

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
