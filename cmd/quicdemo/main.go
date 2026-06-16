package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"log"
	"math/big"
	"sync"
	"time"

	"storj.io/drpc"
	"storj.io/drpc/drpcconn"
	"storj.io/drpc/drpcquic"
	"storj.io/drpc/drpcserver"
)

// trivial string message + encoding (stands in for protobuf)
type msg struct{ S string }
type enc struct{}

func (enc) Marshal(m drpc.Message) ([]byte, error)   { return []byte(m.(*msg).S), nil }
func (enc) Unmarshal(b []byte, m drpc.Message) error { m.(*msg).S = string(b); return nil }

// a handler that echoes the request
type echo struct{}

func (echo) HandleRPC(stream drpc.Stream, rpc string) error {
	in := new(msg)
	if err := stream.MsgRecv(in, enc{}); err != nil {
		return err
	}
	return stream.MsgSend(&msg{S: "echo:" + in.S}, enc{})
}

func devTLS() (server, client *tls.Config) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "demo"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	server = &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{drpcquic.ALPN}}
	client = &tls.Config{InsecureSkipVerify: true, NextProtos: []string{drpcquic.ALPN}} //nolint:gosec
	return
}

func main() {
	ctx := context.Background()
	serverTLS, clientTLS := devTLS()

	ln, err := drpcquic.Listen("127.0.0.1:0", serverTLS, drpcquic.Options{})
	if err != nil {
		log.Fatal(err)
	}
	go func() { _ = drpcquic.Serve(ctx, ln, drpcserver.New(echo{}), drpcquic.Options{}) }()

	mt, err := drpcquic.Dial(ctx, ln.Addr().String(), clientTLS, drpcquic.Options{})
	if err != nil {
		log.Fatal(err)
	}
	conn := drpcconn.NewFromMultiplexed(mt, drpcconn.Options{})
	defer conn.Close()

	var wg sync.WaitGroup
	for i := range 5 { // 5 RPCs concurrently, each on its own QUIC stream
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out := new(msg)
			if err := conn.Invoke(ctx, "/echo", enc{}, &msg{S: fmt.Sprintf("req-%d", i)}, out); err != nil {
				log.Printf("RPC %d error: %v", i, err)
				return
			}
			fmt.Printf("RPC %d -> %s\n", i, out.S)
		}(i)
	}
	wg.Wait()
	fmt.Println("done — drpc is running over QUIC")
}