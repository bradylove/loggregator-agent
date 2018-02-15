package v2

import (
	"context"
	"fmt"
	"io"
	"log"

	"google.golang.org/grpc/codes"

	"code.cloudfoundry.org/go-loggregator/rpc/loggregator_v2"
	plumbing "code.cloudfoundry.org/loggregator-agent/pkg/plumbing/v2"

	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

type HealthRegistrar interface {
	Inc(name string)
	Dec(name string)
}

type SenderFetcher struct {
	opts   []grpc.DialOption
	health HealthRegistrar
}

func NewSenderFetcher(r HealthRegistrar, opts ...grpc.DialOption) *SenderFetcher {
	return &SenderFetcher{
		opts:   opts,
		health: r,
	}
}

func (p *SenderFetcher) Fetch(addr string) (io.Closer, loggregator_v2.Ingress_BatchSenderClient, error) {
	conn, err := grpc.Dial(addr, p.opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("error dialing ingestor stream to %s: %s", addr, err)
	}

	sender, err := openStream(conn)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}

	p.health.Inc("dopplerConnections")
	p.health.Inc("dopplerV2Streams")

	log.Printf("successfully established a stream to doppler %s", addr)

	closer := &decrementingCloser{
		closer: conn,
		health: p.health,
	}
	return closer, sender, err
}

func openStream(conn *grpc.ClientConn) (loggregator_v2.Ingress_BatchSenderClient, error) {
	client := loggregator_v2.NewIngressClient(conn)
	sender, err := client.BatchSender(context.Background())
	if err != nil {
		return nil, fmt.Errorf("error establishing ingestor stream to: %s", err)
	}

	_, err = sender.CloseAndRecv()
	s, ok := status.FromError(err)
	if ok && s.Code() == codes.Unimplemented {
		log.Printf("failed to open stream, falling back to deprecated API")
		client := plumbing.NewDopplerIngressClient(conn)
		sender, err = client.BatchSender(context.Background())
		if err != nil {
			return nil, fmt.Errorf("error establishing ingestor stream to: %s", err)
		}

		return sender, nil
	}

	sender, err = client.BatchSender(context.Background())
	if err != nil {
		return nil, fmt.Errorf("error establishing ingestor stream to: %s", err)
	}

	return sender, nil
}

type decrementingCloser struct {
	closer io.Closer
	health HealthRegistrar
}

func (d *decrementingCloser) Close() error {
	d.health.Dec("dopplerConnections")
	d.health.Dec("dopplerV2Streams")

	return d.closer.Close()
}
