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
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/metadata"
	authv1 "k8s.io/api/authentication/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"
	"sigs.k8s.io/apiserver-network-proxy/pkg/server/metrics"
	"sigs.k8s.io/apiserver-network-proxy/proto/agent"
	"sigs.k8s.io/apiserver-network-proxy/proto/header"
)

// ProxyClientConnection...
type ProxyClientConnection struct {
	Mode      string
	Grpc      client.ProxyService_ProxyServer
	HTTP      net.Conn
	connected chan struct{}
	connectID int64
	agentID   string
	start     time.Time
	backend   Backend
}

func (c *ProxyClientConnection) send(pkt *client.Packet) error {
	if c.Mode == "grpc" {
		stream := c.Grpc
		return stream.Send(pkt)
	} else if c.Mode == "http-connect" {
		if pkt.Type == client.PacketType_CLOSE_RSP {
			return c.HTTP.Close()
		} else if pkt.Type == client.PacketType_DATA {
			_, err := c.HTTP.Write(pkt.GetData().Data)
			return err
		} else if pkt.Type == client.PacketType_DIAL_RSP {
			if pkt.GetDialResponse().Error != "" {
				return c.HTTP.Close()
			}
			return nil
		} else {
			return fmt.Errorf("attempt to send via unrecognized connection type %v", pkt.Type)
		}
	} else {
		return fmt.Errorf("attempt to send via unrecognized connection mode %q", c.Mode)
	}
}

func NewPendingDialManager() *PendingDialManager {
	return &PendingDialManager{
		pendingDial: make(map[int64]*ProxyClientConnection),
	}
}

type PendingDialManager struct {
	mu          sync.RWMutex
	pendingDial map[int64]*ProxyClientConnection
}

func (pm *PendingDialManager) Add(random int64, clientConn *ProxyClientConnection) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.pendingDial[random] = clientConn
}

func (pm *PendingDialManager) Get(random int64) (*ProxyClientConnection, bool) {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	clientConn, ok := pm.pendingDial[random]
	return clientConn, ok
}

func (pm *PendingDialManager) Remove(random int64) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	delete(pm.pendingDial, random)
}

// ProxyServer
type ProxyServer struct {
	// BackendManager manages the backends.
	BackendManager BackendManager

	// Readiness reports if the proxy server is ready, i.e., if the proxy
	// server has connections to proxy agents (backends). Note that the
	// proxy server does not check the healthiness of the connections,
	// though the proxy agents do, so this readiness check might report
	// ready but there is no healthy connection.
	Readiness ReadinessManager

	// fmu protects frontends.
	fmu sync.RWMutex
	// conn = Frontend[agentID][connID]
	frontends map[string]map[int64]*ProxyClientConnection

	PendingDial *PendingDialManager

	serverID    string // unique ID of this server
	serverCount int    // Number of proxy server instances, should be 1 unless it is a HA server.

	// agent authentication
	AgentAuthenticationOptions *AgentTokenAuthenticationOptions
}

// AgentTokenAuthenticationOptions contains list of parameters required for agent token based authentication
type AgentTokenAuthenticationOptions struct {
	Enabled                bool
	AgentNamespace         string
	AgentServiceAccount    string
	AuthenticationAudience string
	KubernetesClient       kubernetes.Interface
}

var _ agent.AgentServiceServer = &ProxyServer{}

var _ client.ProxyServiceServer = &ProxyServer{}

func (s *ProxyServer) addFrontend(agentID string, connID int64, p *ProxyClientConnection) {
	klog.Infof("register frontend %v for agentID %s, connID %v", p, agentID, connID)
	s.fmu.Lock()
	defer s.fmu.Unlock()
	if _, ok := s.frontends[agentID]; !ok {
		s.frontends[agentID] = make(map[int64]*ProxyClientConnection)
	}
	s.frontends[agentID][connID] = p
}

func (s *ProxyServer) removeFrontend(agentID string, connID int64) {
	s.fmu.Lock()
	defer s.fmu.Unlock()
	conns, ok := s.frontends[agentID]
	if !ok {
		klog.Warningf("can't find agentID %s in the frontends", agentID)
		return
	}
	if _, ok := conns[connID]; !ok {
		klog.Warningf("can't find connID %d in the frontends[%s]", connID, agentID)
		return
	}
	klog.Infof("remove frontend %v for agentID %s, connID %v", conns[connID], agentID, connID)
	delete(s.frontends[agentID], connID)
	if len(s.frontends[agentID]) == 0 {
		delete(s.frontends, agentID)
	}
	return
}

func (s *ProxyServer) getFrontend(agentID string, connID int64) (*ProxyClientConnection, error) {
	s.fmu.RLock()
	defer s.fmu.RUnlock()
	conns, ok := s.frontends[agentID]
	if !ok {
		return nil, fmt.Errorf("can't find agentID %s in the frontends", agentID)
	}
	conn, ok := conns[connID]
	if !ok {
		return nil, fmt.Errorf("can't find connID %d in the frontends[%s]", connID, agentID)
	}
	return conn, nil
}

func (s *ProxyServer) getFrontendsForBackendConn(agentID string, backend Backend) ([]*ProxyClientConnection, error) {
	var ret []*ProxyClientConnection
	s.fmu.RLock()
	defer s.fmu.RUnlock()
	frontends, ok := s.frontends[agentID]
	if !ok {
		return nil, fmt.Errorf("can't find agentID %s in the frontends", agentID)
	}
	for _, frontend := range frontends {
		if frontend.backend == backend {
			ret = append(ret, frontend)
		}
	}
	return ret, nil
}

// NewProxyServer creates a new ProxyServer instance
func NewProxyServer(proxyStrategy, serverID string, serverCount int, agentAuthenticationOptions *AgentTokenAuthenticationOptions) *ProxyServer {
	var bm BackendManager
	bs := NewDefaultBackendStorage()
	switch proxyStrategy {
	case "designating":
		bm = NewDesignatingBackendManager(bs)
	default:
		bm = NewDefaultBackendManager(bs)
	}
	rm := NewDefaultReadinessManager(bs)
	return &ProxyServer{
		frontends:                  make(map[string](map[int64]*ProxyClientConnection)),
		PendingDial:                NewPendingDialManager(),
		serverID:                   serverID,
		serverCount:                serverCount,
		BackendManager:             bm,
		AgentAuthenticationOptions: agentAuthenticationOptions,
		Readiness:                  rm,
	}
}

// Proxy handles incoming streams from gRPC frontend.
func (s *ProxyServer) Proxy(stream client.ProxyService_ProxyServer) error {
	md, ok := metadata.FromIncomingContext(stream.Context())
	if !ok {
		return fmt.Errorf("failed to get context")
	}
	userAgent := md.Get(header.UserAgent)
	klog.Infof("proxy request from client, userAgent %s", userAgent)

	recvCh := make(chan *client.Packet, 10)
	stopCh := make(chan error)

	go s.serveRecvFrontend(stream, recvCh)

	defer func() {
		close(recvCh)
	}()

	// Start goroutine to receive packets from frontend and push to recvCh
	go func() {
		for {
			in, err := stream.Recv()
			if err == io.EOF {
				close(stopCh)
				return
			}
			if err != nil {
				klog.Warningf(">>> Stream read from frontend error: %v", err)
				close(stopCh)
				return
			}

			recvCh <- in
		}
	}()

	return <-stopCh
}

func (s *ProxyServer) serveRecvFrontend(stream client.ProxyService_ProxyServer, recvCh <-chan *client.Packet) {
	klog.Info("start serving frontend stream")

	var firstConnID int64
	// The first packet should be a DIAL_REQ, we will randomly get a
	// backend from the BackendManger then.
	var backend Backend
	var err error

	for pkt := range recvCh {
		switch pkt.Type {
		case client.PacketType_DIAL_REQ:
			klog.Info(">>> Received DIAL_REQ")
			// TODO: if we track what agent has historically served
			// the address, then we can send the Dial_REQ to the
			// same agent. That way we save the agent from creating
			// a new connection to the address.
			backend, err = s.BackendManager.Backend()
			if err != nil {
				klog.Errorf(">>> failed to get a backend: %v", err)
				continue
			}
			s.PendingDial.Add(
				pkt.GetDialRequest().Random,
				&ProxyClientConnection{
					Mode:      "grpc",
					Grpc:      stream,
					connected: make(chan struct{}),
					start:     time.Now(),
					backend:   backend,
				})
			if err := backend.Send(pkt); err != nil {
				klog.Warningf(">>> DIAL_REQ to Backend failed: %v", err)
			}
			klog.Info(">>> DIAL_REQ sent to backend") // got this. but backend didn't receive anything.

		case client.PacketType_CLOSE_REQ:
			connID := pkt.GetCloseRequest().ConnectID
			klog.Infof(">>> Received CLOSE_REQ(id=%d)", connID)
			if backend == nil {
				klog.Errorf("backend has not been initialized for connID %d. Client should send a Dial Request first.", connID)
				continue
			}
			if err := backend.Send(pkt); err != nil {
				// TODO: retry with other backends connecting to this agent.
				klog.Warningf(">>> CLOSE_REQ to Backend failed: %v", err)
			}
			klog.Info(">>> CLOSE_REQ sent to backend")

		case client.PacketType_DATA:
			connID := pkt.GetData().ConnectID
			data := pkt.GetData().Data
			klog.Infof(">>> Received %d bytes of DATA(id=%d)", len(data), connID)
			if firstConnID == 0 {
				firstConnID = connID
			} else if firstConnID != connID {
				klog.Warningf(">>> Data(id=%d) doesn't match first connection id %d", firstConnID, connID)
			}

			if backend == nil {
				klog.Errorf("backend has not been initialized for connID %d. Client should send a Dial Request first.", connID)
				continue
			}
			if err := backend.Send(pkt); err != nil {
				// TODO: retry with other backends connecting to this agent.
				klog.Warningf(">>> DATA to Backend failed: %v", err)
				continue
			}
			klog.Info(">>> DATA sent to Backend")

		default:
			klog.Infof(">>> Ignore %v packet coming from frontend", pkt.Type)
		}
	}

	klog.Infof(">>> Close streaming (id=%d)", firstConnID)

	pkt := &client.Packet{
		Type: client.PacketType_CLOSE_REQ,
		Payload: &client.Packet_CloseRequest{
			CloseRequest: &client.CloseRequest{
				ConnectID: firstConnID,
			},
		},
	}

	if backend == nil {
		klog.Errorf("backend has not been initialized for connID %d. Client should send a Dial Request first.", firstConnID)
		return
	}
	if err := backend.Send(pkt); err != nil {
		klog.Warningf(">>> CLOSE_REQ to Backend failed: %v", err)
	}
}

func (s *ProxyServer) serveSend(stream client.ProxyService_ProxyServer, sendCh <-chan *client.Packet) {
	klog.Info("start serve send ...")
	for pkt := range sendCh {
		err := stream.Send(pkt)
		if err != nil {
			klog.Warningf("stream write error: %v", err)
		}
	}
}

func agentID(stream agent.AgentService_ConnectServer) (string, error) {
	md, ok := metadata.FromIncomingContext(stream.Context())
	if !ok {
		return "", fmt.Errorf("failed to get context")
	}
	agentIDs := md.Get(header.AgentID)
	if len(agentIDs) != 1 {
		return "", fmt.Errorf("expected one agent ID in the context, got %v", agentIDs)
	}
	return agentIDs[0], nil
}

func (s *ProxyServer) validateAuthToken(token string) error {
	trReq := &authv1.TokenReview{
		Spec: authv1.TokenReviewSpec{
			Token:     token,
			Audiences: []string{s.AgentAuthenticationOptions.AuthenticationAudience},
		},
	}
	r, err := s.AgentAuthenticationOptions.KubernetesClient.AuthenticationV1().TokenReviews().Create(trReq)
	if err != nil {
		return fmt.Errorf("Failed to authenticate request. err:%v", err)
	}

	if r.Status.Error != "" {
		return fmt.Errorf("lookup failed: %s", r.Status.Error)
	}

	if !r.Status.Authenticated {
		return fmt.Errorf("lookup failed: service account jwt not valid")
	}

	// The username is of format: system:serviceaccount:(NAMESPACE):(SERVICEACCOUNT)
	parts := strings.Split(r.Status.User.Username, ":")
	if len(parts) != 4 {
		return fmt.Errorf("lookup failed: unexpected username format")
	}
	// Validate the user that comes back from token review is a service account
	if parts[0] != "system" || parts[1] != "serviceaccount" {
		return fmt.Errorf("lookup failed: username returned is not a service account")
	}

	ns := parts[2]
	sa := parts[3]
	if s.AgentAuthenticationOptions.AgentNamespace != ns {
		return fmt.Errorf("lookup failed: incoming request from %q namespace. Expected %q", ns, s.AgentAuthenticationOptions.AgentNamespace)
	}

	if s.AgentAuthenticationOptions.AgentServiceAccount != sa {
		return fmt.Errorf("lookup failed: incoming request from %q service account. Expected %q", sa, s.AgentAuthenticationOptions.AgentServiceAccount)
	}

	return nil
}

func (s *ProxyServer) authenticateAgentViaToken(ctx context.Context) error {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return fmt.Errorf("Failed to retrieve metadata from context")
	}

	authContext := md.Get(header.AuthenticationTokenContextKey)
	if len(authContext) == 0 {
		return fmt.Errorf("Authentication context was not found in metadata")
	}

	if len(authContext) > 1 {
		return fmt.Errorf("too many (%d) tokens are received", len(authContext))
	}

	if !strings.HasPrefix(authContext[0], header.AuthenticationTokenContextSchemePrefix) {
		return fmt.Errorf("received token does not have %q prefix", header.AuthenticationTokenContextSchemePrefix)
	}

	if err := s.validateAuthToken(strings.TrimPrefix(authContext[0], header.AuthenticationTokenContextSchemePrefix)); err != nil {
		return fmt.Errorf("Failed to validate authentication token, err:%v", err)
	}

	klog.Infof("Client successfully authenticated via token")
	return nil
}

// Connect is for agent to connect to ProxyServer as next hop
func (s *ProxyServer) Connect(stream agent.AgentService_ConnectServer) error {
	agentID, err := agentID(stream)
	if err != nil {
		return err
	}
	klog.Infof("Connect request from agent %s", agentID)
	backend := s.BackendManager.AddBackend(agentID, stream)
	defer s.BackendManager.RemoveBackend(agentID, stream)

	h := metadata.Pairs(header.ServerID, s.serverID, header.ServerCount, strconv.Itoa(s.serverCount))
	if err := stream.SendHeader(h); err != nil {
		return err
	}

	recvCh := make(chan *client.Packet, 10)
	stopCh := make(chan error)

	if s.AgentAuthenticationOptions.Enabled {
		if err := s.authenticateAgentViaToken(stream.Context()); err != nil {
			klog.Infof("Client authentication failed. err:%v", err)
			return err
		}
	}

	go s.serveRecvBackend(backend, stream, agentID, recvCh)

	defer func() {
		close(recvCh)
	}()

	go func() {
		for {
			in, err := stream.Recv()
			if err == io.EOF {
				close(stopCh)
				return
			}
			if err != nil {
				klog.Warningf("stream read error: %v", err)
				close(stopCh)
				return
			}

			recvCh <- in
		}
	}()

	return <-stopCh
}

// route the packet back to the correct client
func (s *ProxyServer) serveRecvBackend(backend Backend, stream agent.AgentService_ConnectServer, agentID string, recvCh <-chan *client.Packet) {
	defer func() {
		// Close all connected frontends when the agent connection is closed
		// TODO(#126): Frontends in PendingDial state that have not been added to the
		//             list of frontends should also be closed.
		frontends, _ := s.getFrontendsForBackendConn(agentID, backend)
		klog.Infof("<<< Close %d frontends connected to agent %s", len(frontends), agentID)

		for _, frontend := range frontends {
			s.removeFrontend(agentID, frontend.connectID)
			pkt := &client.Packet{
				Type: client.PacketType_CLOSE_RSP,
				Payload: &client.Packet_CloseResponse{
					CloseResponse: &client.CloseResponse{},
				},
			}
			pkt.GetCloseResponse().ConnectID = frontend.connectID
			if err := frontend.send(pkt); err != nil {
				klog.Warningf("<<< CLOSE_RSP to frontend failed: %v", err)
			}
		}
	}()

	for pkt := range recvCh {
		switch pkt.Type {
		case client.PacketType_DIAL_RSP:
			resp := pkt.GetDialResponse()
			klog.Infof("<<< Received DIAL_RSP(rand=%d), agentID %s, connID %d)", resp.Random, agentID, resp.ConnectID)

			if frontend, ok := s.PendingDial.Get(resp.Random); !ok {
				klog.Warning("<<< DialResp not recognized; dropped")
			} else {
				dialErr := false
				if resp.Error != "" {
					klog.Warningf("<<< DIAL_RSP contains error: %v", resp.Error)
					dialErr = true
				}
				err := frontend.send(pkt)
				s.PendingDial.Remove(resp.Random)
				if err != nil {
					klog.Warningf("<<< DIAL_RSP send to frontend stream error: %v", err)
					dialErr = true
				}
				// Avoid adding the frontend if there was an error dialing the destination
				if dialErr == true {
					break
				}
				frontend.connectID = resp.ConnectID
				frontend.agentID = agentID
				s.addFrontend(agentID, resp.ConnectID, frontend)
				close(frontend.connected)
				metrics.Metrics.ObserveDialLatency(time.Since(frontend.start))
			}

		case client.PacketType_DATA:
			resp := pkt.GetData()
			klog.Infof("<<< Received %d bytes of DATA from agentID %s, connID %d", len(resp.Data), agentID, resp.ConnectID)
			frontend, err := s.getFrontend(agentID, resp.ConnectID)
			if err != nil {
				klog.Warning(err)
				break
			}
			if err := frontend.send(pkt); err != nil {
				klog.Warningf("<<< DATA send to client stream error: %v", err)
			} else {
				klog.V(6).Infof("<<< DATA sent to frontend")
			}

		case client.PacketType_CLOSE_RSP:
			resp := pkt.GetCloseResponse()
			klog.Infof("<<< Received CLOSE_RSP(id=%d)", resp.ConnectID)
			frontend, err := s.getFrontend(agentID, resp.ConnectID)
			if err != nil {
				klog.Warning(err)
				break
			}
			if err := frontend.send(pkt); err != nil {
				// Normal when frontend closes it.
				klog.Infof("<<< CLOSE_RSP send to client stream error: %v", err)
			} else {
				klog.Infof("<<< CLOSE_RSP sent to frontend")
			}
			s.removeFrontend(agentID, resp.ConnectID)
			klog.Infof("<<< Close streaming (agentID=%s, connId=%d)", agentID, resp.ConnectID)

		default:
			klog.Warningf("<<< Unrecognized packet %+v", pkt)
		}
	}
	klog.Infof("<<< Close backend %v of agent %s", stream, agentID)
}
