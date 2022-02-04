package loop

import (
	"context"
	"fmt"
	"sync"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/zpay32"
)

var (
	// ErrRoutingPluginNotApplicable means that the selected routing plugin
	// is not able to enhance routing given the current conditions and
	// therefore shouldn't be used.
	ErrRoutingPluginNotApplicable = fmt.Errorf("routing plugin not " +
		"applicable")

	// ErrRoutingPluginNoMoreRetries means that the routing plugin can't
	// effectively help the payment with more retries.
	ErrRoutingPluginNoMoreRetries = fmt.Errorf("routing plugin can't " +
		"retry more")
)

var (
	routingPluginMx       sync.Mutex
	routingPluginInstance RoutingPlugin
)

// RoutingPlugin is a generic interface for off-chain payment helpers.
type RoutingPlugin interface {
	// Init initializes the routing plugin.
	Init(ctx context.Context, target route.Vertex,
		routeHints [][]zpay32.HopHint, amt btcutil.Amount) error

	// Done deinitializes the routing plugin (restoring any state the
	// plugin might have changed).
	Done(ctx context.Context) error

	// BeforePayment is called before each payment. Attempt counter is
	// passed, counting attempts from 1.
	BeforePayment(ctx context.Context, attempt int, maxAttempts int) error
}

// makeRoutingPlugin is a helper to instantiate routing plugins.
func makeRoutingPlugin(plugin RoutingPluginType,
	_ lndclient.LndServices) RoutingPlugin {

	return nil
}

// AcquireRoutingPlugin will return a RoutingPlugin instance (or nil). As the
// LND instance used is a shared resource, currently only one requestor will be
// able to acquire a RoutingPlugin instance. If someone is already holding the
// instance a nil is returned.
func AcquireRoutingPlugin(ctx context.Context, pluginType RoutingPluginType,
	lnd lndclient.LndServices, target route.Vertex,
	routeHints [][]zpay32.HopHint, amt btcutil.Amount) (
	RoutingPlugin, error) {

	routingPluginMx.Lock()
	defer routingPluginMx.Unlock()

	// Another swap is already using the routing plugin.
	if routingPluginInstance != nil {
		return nil, nil
	}

	routingPluginInstance = makeRoutingPlugin(pluginType, lnd)
	if routingPluginInstance == nil {
		return nil, nil
	}

	// Initialize the plugin with the passed parameters.
	err := routingPluginInstance.Init(ctx, target, routeHints, amt)
	if err != nil {
		if err == ErrRoutingPluginNotApplicable {
			// Since the routing plugin is not applicable for this
			// payment, we can immediately destruct it.
			if err := routingPluginInstance.Done(ctx); err != nil {
				log.Errorf("Error while releasing routing "+
					"plugin: %v", err)
			}

			// ErrRoutingPluginNotApplicable is non critical, so
			// we're masking this error as we can continue the swap
			// flow without the routing plugin.
			err = nil
		}

		routingPluginInstance = nil
		return nil, err
	}

	return routingPluginInstance, nil
}

// ReleaseRoutingPlugin will release the RoutingPlugin, allowing other
// requestors to acquire the instance.
func ReleaseRoutingPlugin(ctx context.Context) {
	routingPluginMx.Lock()
	defer routingPluginMx.Unlock()

	if routingPluginInstance == nil {
		return
	}

	if err := routingPluginInstance.Done(ctx); err != nil {
		log.Errorf("Error while releasing routing plugin: %v",
			err)
	}

	routingPluginInstance = nil
}
