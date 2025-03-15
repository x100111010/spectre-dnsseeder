package netadapter

import (
	"sync"

	"github.com/spectre-project/spectred/app/protocol/common"
	"github.com/spectre-project/spectred/util/mstime"

	"github.com/spectre-project/spectred/infrastructure/network/netadapter/id"

	"github.com/spectre-project/spectred/app/appmessage"
	"github.com/spectre-project/spectred/infrastructure/network/netadapter/router"

	"github.com/spectre-project/spectred/infrastructure/config"
	"github.com/spectre-project/spectred/infrastructure/network/netadapter"

	"github.com/pkg/errors"
)

// DnsseedNetAdapter allows tests and other tools to use a simple network adapter without implementing
// all the required supporting structures.
type DnsseedNetAdapter struct {
	cfg        *config.Config
	lock       sync.Mutex
	netAdapter *netadapter.NetAdapter
	routesChan <-chan *Routes
}

// NewDnsseedNetAdapter creates a new instance of a DnsseedNetAdapter
func NewDnsseedNetAdapter(cfg *config.Config) (*DnsseedNetAdapter, error) {
	netAdapter, err := netadapter.NewNetAdapter(cfg)
	if err != nil {
		return nil, errors.Wrap(err, "Error starting netAdapter")
	}

	routerInitializer, routesChan := generateRouteInitializer()

	netAdapter.SetP2PRouterInitializer(routerInitializer)
	netAdapter.SetRPCRouterInitializer(func(_ *router.Router, _ *netadapter.NetConnection) {
	})

	err = netAdapter.Start()
	if err != nil {
		return nil, errors.Wrap(err, "Error starting netAdapter")
	}

	return &DnsseedNetAdapter{
		cfg:        cfg,
		lock:       sync.Mutex{},
		netAdapter: netAdapter,
		routesChan: routesChan,
	}, nil
}

// Connect opens a connection to the given address, handles handshake, and returns the routes for this connection
// To simplify usage the return type contains only two routes:
// OutgoingRoute - for all outgoing messages
// IncomingRoute - for all incoming messages (excluding handshake messages)
func (mna *DnsseedNetAdapter) Connect(address string) (*Routes, *appmessage.MsgVersion, error) {
	mna.lock.Lock()
	defer mna.lock.Unlock()

	err := mna.netAdapter.P2PConnect(address)
	if err != nil {
		return nil, nil, err
	}

	routes := <-mna.routesChan
	msgVersion, err := mna.handleHandshake(routes, mna.netAdapter.ID())
	if err != nil {
		return nil, nil, errors.Wrap(err, "Error in handshake")
	}

	spawn("netAdapterMock-handlePingPong", func() {
		err := mna.handlePingPong(routes)
		if err != nil {
			panic(errors.Wrap(err, "Error from ping-pong"))
		}
	})

	return routes, msgVersion, nil
}

// handlePingPong makes sure that we are not disconnected due to not responding to pings.
// However, it only responds to pings, not sending its own, to conform to the minimal-ness
// of DnsseedNetAdapter
func (*DnsseedNetAdapter) handlePingPong(routes *Routes) error {
	for {
		message, err := routes.pingRoute.Dequeue()
		if err != nil {
			if errors.Is(err, router.ErrRouteClosed) {
				return nil
			}
			return err
		}

		pingMessage := message.(*appmessage.MsgPing)

		err = routes.OutgoingRoute.Enqueue(&appmessage.MsgPong{Nonce: pingMessage.Nonce})
		if err != nil {
			return err
		}
	}
}

func (mna *DnsseedNetAdapter) handleHandshake(routes *Routes, ourID *id.ID) (*appmessage.MsgVersion, error) {
	msg, err := routes.handshakeRoute.DequeueWithTimeout(common.DefaultTimeout)
	if err != nil {
		return nil, err
	}
	msgVersion, ok := msg.(*appmessage.MsgVersion)
	if !ok {
		return nil, errors.Errorf("expected first message to be of type %s, but got %s", appmessage.CmdVersion, msg.Command())
	}
	err = routes.OutgoingRoute.Enqueue(&appmessage.MsgVersion{
		ProtocolVersion: msgVersion.ProtocolVersion,
		Network:         mna.cfg.ActiveNetParams.Name,
		Services:        msgVersion.Services,
		Timestamp:       mstime.Now(),
		Address:         nil,
		ID:              ourID,
		UserAgent:       "/net-adapter-mock/",
		DisableRelayTx:  true,
		SubnetworkID:    nil,
	})
	if err != nil {
		return msgVersion, err
	}

	msg, err = routes.handshakeRoute.DequeueWithTimeout(common.DefaultTimeout)
	if err != nil {
		return msgVersion, err
	}
	_, ok = msg.(*appmessage.MsgVerAck)
	if !ok {
		return msgVersion, errors.Errorf("expected second message to be of type %s, but got %s", appmessage.CmdVerAck, msg.Command())
	}
	err = routes.OutgoingRoute.Enqueue(&appmessage.MsgVerAck{})
	if err != nil {
		return msgVersion, err
	}

	msg, err = routes.addressesRoute.DequeueWithTimeout(common.DefaultTimeout)
	if err != nil {
		return msgVersion, err
	}
	_, ok = msg.(*appmessage.MsgRequestAddresses)
	if !ok {
		return msgVersion, errors.Errorf("expected third message to be of type %s, but got %s", appmessage.CmdRequestAddresses, msg.Command())
	}
	err = routes.OutgoingRoute.Enqueue(&appmessage.MsgAddresses{
		AddressList: []*appmessage.NetAddress{},
	})
	if err != nil {
		return msgVersion, err
	}

	err = routes.OutgoingRoute.Enqueue(&appmessage.MsgRequestAddresses{
		IncludeAllSubnetworks: true,
		SubnetworkID:          nil,
	})
	if err != nil {
		return msgVersion, err
	}
	msg, err = routes.addressesRoute.DequeueWithTimeout(common.DefaultTimeout)
	if err != nil {
		return msgVersion, err
	}
	_, ok = msg.(*appmessage.MsgAddresses)
	if !ok {
		return msgVersion, errors.Errorf("expected fourth message to be of type %s, but got %s", appmessage.CmdAddresses, msg.Command())
	}

	return msgVersion, nil
}

func generateRouteInitializer() (netadapter.RouterInitializer, <-chan *Routes) {
	cmdsWithBuiltInRoutes := []appmessage.MessageCommand{
		appmessage.CmdVersion,
		appmessage.CmdVerAck,
		appmessage.CmdRequestAddresses,
		appmessage.CmdAddresses,
		appmessage.CmdPing}

	everythingElse := make([]appmessage.MessageCommand, 0, len(appmessage.ProtocolMessageCommandToString)-len(cmdsWithBuiltInRoutes))
outerLoop:
	for command := range appmessage.ProtocolMessageCommandToString {
		for _, cmdWithBuiltInRoute := range cmdsWithBuiltInRoutes {
			if command == cmdWithBuiltInRoute {
				continue outerLoop
			}
		}

		everythingElse = append(everythingElse, command)
	}

	routesChan := make(chan *Routes)

	routeInitializer := func(router *router.Router, netConnection *netadapter.NetConnection) {
		handshakeRoute, err := router.AddIncomingRoute("handshake", []appmessage.MessageCommand{appmessage.CmdVersion, appmessage.CmdVerAck})
		if err != nil {
			panic(errors.Wrap(err, "error registering handshake route"))
		}
		addressesRoute, err := router.AddIncomingRoute("addresses", []appmessage.MessageCommand{appmessage.CmdRequestAddresses, appmessage.CmdAddresses})
		if err != nil {
			panic(errors.Wrap(err, "error registering addresses route"))
		}
		pingRoute, err := router.AddIncomingRoute("ping", []appmessage.MessageCommand{appmessage.CmdPing})
		if err != nil {
			panic(errors.Wrap(err, "error registering ping route"))
		}
		everythingElseRoute, err := router.AddIncomingRoute("everything else", everythingElse)
		if err != nil {
			panic(errors.Wrap(err, "error registering everythingElseRoute"))
		}

		err = router.OutgoingRoute().Enqueue(appmessage.NewMsgReady())
		if err != nil {
			panic(errors.Wrap(err, "error sending ready message"))
		}

		spawn("netAdapterMock-routeInitializer-sendRoutesToChan", func() {
			routesChan <- &Routes{
				netConnection:  netConnection,
				OutgoingRoute:  router.OutgoingRoute(),
				IncomingRoute:  everythingElseRoute,
				handshakeRoute: handshakeRoute,
				addressesRoute: addressesRoute,
				pingRoute:      pingRoute,
			}
		})
	}

	return routeInitializer, routesChan
}
