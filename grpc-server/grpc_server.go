package grpcserver

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	grpcmiddleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpcprometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/packethost/pkg/log"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/tinkerbell/hegel/grpc/protos/hegel"
	"github.com/tinkerbell/hegel/hardware"
	"github.com/tinkerbell/hegel/metrics"
	"github.com/tinkerbell/hegel/xff"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

//go:generate protoc -I grpc/protos grpc/protos/hegel.proto --go_out=plugins=grpc:grpc/hegel

type Server struct {
	log            log.Logger
	hardwareClient hardware.Client

	subscriptionsM *sync.RWMutex
	subscriptions  map[string]*Subscription
}

type Subscription struct {
	ID           string        `json:"id"`
	IP           string        `json:"ip"`
	InitDuration time.Duration `json:"init_duration"`
	StartedAt    time.Time     `json:"started_at"`
	cancel       func()
	updateChan   chan []byte
}

func NewServer(l log.Logger, hc hardware.Client) *Server {
	return &Server{
		log:            l,
		hardwareClient: hc,
		subscriptionsM: &sync.RWMutex{},
		subscriptions:  make(map[string]*Subscription),
	}
}

func Serve(_ context.Context, l log.Logger, srv *Server, port int, unparsedProxies, tlsCertPath, tlsKeyPath string, useTLS bool) error {
	serverOpts := make([]grpc.ServerOption, 0)

	if useTLS {
		creds, err := credentials.NewServerTLSFromFile(tlsCertPath, tlsKeyPath)
		if err != nil {
			l.Error(err, "failed to initialize server credentials")
			panic(err)
		}
		serverOpts = append(serverOpts, grpc.Creds(creds))
	}

	proxies := xff.ParseTrustedProxies(unparsedProxies)
	xffStream, xffUnary := xff.GRPCMiddlewares(l, proxies)
	streamLogger, unaryLogger := l.GRPCLoggers()
	serverOpts = append(serverOpts,
		grpcmiddleware.WithUnaryServerChain(
			xffUnary,
			unaryLogger,
			grpcprometheus.UnaryServerInterceptor,
			otelgrpc.UnaryServerInterceptor(),
		),
		grpcmiddleware.WithStreamServerChain(
			xffStream,
			streamLogger,
			grpcprometheus.StreamServerInterceptor,
			otelgrpc.StreamServerInterceptor(),
		),
	)

	grpcServer := grpc.NewServer(serverOpts...)

	grpcprometheus.Register(grpcServer)

	hegel.RegisterHegelServer(grpcServer, srv)

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		err = errors.Wrap(err, "failed to listen")
		l.Error(err)
		panic(err)
	}

	metrics.State.Set(metrics.Ready)
	l.Info("serving grpc")
	err = grpcServer.Serve(lis)
	if err != nil {
		l.Fatal(err, "failed to serve grpc")
	}

	return nil
}

func (s *Server) Subscriptions() map[string]Subscription {
	subscriptions := make(map[string]Subscription, len(s.subscriptions))

	for key, subscription := range s.subscriptions {
		subscriptions[key] = *subscription
	}

	return subscriptions
}

func (s *Server) Get(ctx context.Context, _ *hegel.GetRequest) (*hegel.GetResponse, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil, errors.New("could not get peer info from client")
	}
	s.log.With("client", p.Addr, "op", "get").Info()

	ip := extractIP(p.Addr)

	hw, err := s.hardwareClient.ByIP(ctx, ip)
	if err != nil {
		return nil, err
	}
	ehw, err := hw.Export()
	if err != nil {
		return nil, err
	}
	return &hegel.GetResponse{
		JSON: string(ehw),
	}, nil
}

func (s *Server) Subscribe(_ *hegel.SubscribeRequest, stream hegel.Hegel_SubscribeServer) error {
	startedAt := time.Now().UTC()
	metrics.TotalSubscriptions.Inc()
	metrics.Subscriptions.WithLabelValues("initializing").Inc()
	timer := prometheus.NewTimer(metrics.InitDuration)

	logger := s.log.With("op", "subscribe")

	handleError := func(err error) error {
		logger.Error(err)
		metrics.Errors.WithLabelValues("subscribe", "active").Inc()
		metrics.Subscriptions.WithLabelValues("initializing").Dec()
		timer.ObserveDuration()
		return err
	}

	p, ok := peer.FromContext(stream.Context())
	if !ok {
		return handleError(errors.New("could not get peer info from client"))
	}

	ip := extractIP(p.Addr)

	logger = logger.With("ip", ip, "client", p.Addr)
	logger.Info()

	id, err := s.getHardwareID(stream.Context(), ip)
	if err != nil {
		return handleError(err)
	}

	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()

	hardwareUpdatesStream, err := s.hardwareClient.Watch(ctx, id)
	if err != nil {
		return handleError(err)
	}

	newSubscription := &Subscription{
		ID:           id,
		IP:           ip,
		StartedAt:    startedAt,
		InitDuration: time.Since(startedAt),
		cancel:       cancel,
	}

	// We support only a single subscription per hardware so if one exists cancel it's context.
	s.removeSubscriptionIfExists(newSubscription)

	// On exit remove the subscription from the map. Because we only support a single subscription per hardware
	// we need to check if the subscription currently registered in the map is the same as the subscription we
	// were created with. If t is, we can remove ourselves else we want to leave it untouched as it represents
	// a new stream.
	defer s.removeSubscriptionIfExists(newSubscription)

	timer.ObserveDuration()
	metrics.Subscriptions.WithLabelValues("initializing").Dec()
	metrics.Subscriptions.WithLabelValues("active").Inc()
	defer metrics.Subscriptions.WithLabelValues("active").Dec()

	handleError = func(err error) error {
		if err != nil {
			logger.Error(err)
			metrics.Errors.WithLabelValues("subscribe", "active").Inc()
		}
		return err
	}

	for {
		hardwareUpdate, err := hardwareUpdatesStream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}

		if err != nil {
			return handleError(err)
		}

		exportedHardware, err := hardwareUpdate.Export()
		if err != nil {
			return handleError(err)
		}

		if err := stream.Send(&hegel.SubscribeResponse{
			JSON: string(exportedHardware),
		}); err != nil {
			return handleError(err)
		}
	}
}

// removeSubscriptionIfExists removes subscription if its still in the subscriptions map. The subscription in the map
// may be a new subscription that overrode subscription when another client subscribed to the same hardware ID. In
// that case, we leave the subscription alone.
func (s *Server) removeSubscriptionIfExists(subscription *Subscription) {
	s.subscriptionsM.Lock()
	defer s.subscriptionsM.Unlock()

	// Always cancel because its idempotent.
	subscription.cancel()

	if current := s.subscriptions[subscription.ID]; current == subscription {
		delete(s.subscriptions, subscription.ID)
	}
}

func (s *Server) replaceSubscriptionIfExists(subscription *Subscription) {
	s.subscriptionsM.Lock()
	defer s.subscriptionsM.Unlock()
	if current, found := s.subscriptions[subscription.ID]; found {
		current.cancel()
	}
	s.subscriptions[subscription.ID] = subscription
}

func (s *Server) getHardwareID(ctx context.Context, ip string) (string, error) {
	hardwareData, err := s.hardwareClient.ByIP(ctx, ip)
	if err != nil {
		return "", err
	}

	id, err := hardwareData.ID()
	if err != nil {
		return "", err
	}

	return id, nil
}

func extractIP(a net.Addr) string {
	if tcp, ok := a.(*net.TCPAddr); ok {
		return tcp.IP.String()
	}
	return a.String()
}
