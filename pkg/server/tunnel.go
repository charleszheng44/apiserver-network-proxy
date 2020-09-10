/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package server

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"k8s.io/klog/v2"
	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"
)

// Tunnel implements Proxy based on HTTP Connect, which tunnels the traffic to
// the agent registered in ProxyServer.
type Tunnel struct {
	Server *ProxyServer
}

func (t *Tunnel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	klog.Infof("Received %s request to %q from userAgent %s", r.Method, r.Host, r.UserAgent())
	if r.TLS != nil {
		klog.Infof("TLS CommonName: %v", r.TLS.PeerCertificates[0].Subject.CommonName)
	}
	if r.Method != http.MethodConnect {
		http.Error(w, "this proxy only supports CONNECT passthrough", http.StatusMethodNotAllowed)
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)

	conn, bufrw, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	random := rand.Int63()
	dialRequest := &client.Packet{
		Type: client.PacketType_DIAL_REQ,
		Payload: &client.Packet_DialRequest{
			DialRequest: &client.DialRequest{
				Protocol: "tcp",
				Address:  r.Host,
				Random:   random,
			},
		},
	}
	klog.Infof("Set pending(rand=%d) to %v", random, w)
	ctx := context.Background()
	switch t.Server.proxyStrategy {
	case "destIP":
		ip := strings.Split(r.Host, ":")[0]
		ctx = context.WithValue(ctx, DestIPContextKey, ip)
	case "default":
	}
	backend, err := t.Server.BackendManager.Backend(ctx)
	if err != nil {
		http.Error(w, fmt.Sprintf("currently no tunnels available: %v", err), http.StatusInternalServerError)
		return
	}
	connected := make(chan struct{})
	connection := &ProxyClientConnection{
		Mode:      "http-connect",
		HTTP:      conn,
		connected: connected,
		start:     time.Now(),
		backend:   backend,
	}
	t.Server.PendingDial.Add(random, connection)
	if err := backend.Send(dialRequest); err != nil {
		klog.Errorf("failed to tunnel dial request %v", err)
		return
	}
	ctxt := backend.Context()
	if ctxt.Err() != nil {
		klog.Errorf("context reports error %v", err)
	}

	select {
	case <-ctxt.Done():
		klog.Errorf("context reports done!!!")
	default:
	}

	select {
	case <-connection.connected: // Waiting for response before we begin full communication.
	}

	defer conn.Close()

	klog.Infof("Starting proxy to %q", r.Host)
	pkt := make([]byte, 1<<12)

	connID := connection.connectID
	agentID := connection.agentID
	var acc int

	for {
		n, err := bufrw.Read(pkt[:])
		acc += n
		if err == io.EOF {
			klog.Warningf("EOF from %v", r.Host)
			break
		}
		if err != nil {
			klog.Errorf("Received error on connection %v", err)
			break
		}

		packet := &client.Packet{
			Type: client.PacketType_DATA,
			Payload: &client.Packet_Data{
				Data: &client.Data{
					ConnectID: connID,
					Data:      pkt[:n],
				},
			},
		}
		err = backend.Send(packet)
		if err != nil {
			klog.Errorf("error sending packet %v", err)
			break
		}
		klog.Infof("Forwarding %d (total %d) bytes of DATA on tunnel for agentID %s, connID %d", n, acc, connection.agentID, connection.connectID)
	}

	klog.Infof("Stopping transfer to %q, agentID %s, connID %d", r.Host, agentID, connID)
}
