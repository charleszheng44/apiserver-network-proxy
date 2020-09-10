/*
Copyright 2020 The Kubernetes Authors.

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
	"errors"
	"math/rand"
	"sync"
	"time"

	"k8s.io/klog/v2"
	client "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"
	"sigs.k8s.io/apiserver-network-proxy/proto/agent"
)

const DestIPContextKey = "destIP"

type Backend interface {
	Send(p *client.Packet) error
	Context() context.Context
}

var _ Backend = &backend{}
var _ Backend = agent.AgentService_ConnectServer(nil)

type backend struct {
	// TODO: this is a multi-writer single-reader pattern, it's tricky to
	// write it using channel. Let's worry about performance later.
	mu   sync.Mutex // mu protects conn
	conn agent.AgentService_ConnectServer
}

func (b *backend) Send(p *client.Packet) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.conn.Send(p)
}

func (b *backend) Context() context.Context {
	// TODO: does Context require lock protection?
	return b.conn.Context()
}

func newBackend(conn agent.AgentService_ConnectServer) *backend {
	return &backend{conn: conn}
}

// BackendStorage is an interface to manage the storage of the backend
// connections, i.e., get, add and remove
type BackendStorage interface {
	// GetBackend returns a backend associating to the agentID.
	GetBackend(agentID string) Backend
	// AddBackend adds a backend.
	AddBackend(agentID string, conn agent.AgentService_ConnectServer) Backend
	// RemoveBackend removes a backend.
	RemoveBackend(agentID string, conn agent.AgentService_ConnectServer)
	// NumBackends returns the number of backends.
	NumBackends() int
}

// BackendManager is an interface to manage backend connections, i.e.,
// connection to the proxy agents.
type BackendManager interface {
	// Backend returns a single backend.
	Backend(context.Context) (Backend, error)
	BackendStorage
}

var _ BackendManager = &DefaultBackendManager{}

// DefaultBackendManager is the default backend manager.
type DefaultBackendManager struct {
	*DefaultBackendStorage
}

func (dbm *DefaultBackendManager) Backend(_ context.Context) (Backend, error) {
	be := dbm.DefaultBackendStorage.GetRandomBackend()
	if be == nil {
		return nil, &ErrNotFound{}
	}
	return be, nil
}

var _BackendManager = &DestIPBackendManager{}

type DestIPBackendManager struct {
	BackendStorage
}

func (dbm *DestIPBackendManager) Backend(ctx context.Context) (Backend, error) {
	destIPInf := ctx.Value(DestIPContextKey)
	if destIPInf == nil {
		return nil, errors.New("no agentID found in the context")
	}
	agentID, ok := destIPInf.(string)
	if !ok {
		return nil, errors.New("type assertion failed")
	}
	be := dbm.BackendStorage.GetBackend(agentID)
	if be == nil {
		return nil, &ErrNotFound{}
	}
	return be, nil
}

// DefaultBackendStorage is the default backend storage.
type DefaultBackendStorage struct {
	mu sync.RWMutex //protects the following
	// A map between agentID and its grpc connections.
	// For a given agent, ProxyServer prefers backends[agentID][0] to send
	// traffic, because backends[agentID][1:] are more likely to be closed
	// by the agent to deduplicate connections to the same server.
	backends map[string][]*backend
	// agentID is tracked in this slice to enable randomly picking an
	// agentID in the Backend() method. There is no reliable way to
	// randomly pick a key from a map (in this case, the backends) in
	// Golang.
	agentIDs []string
	random   *rand.Rand
}

// NewDefaultBackendManager returns a DefaultBackendManager.
func NewDefaultBackendManager(bs *DefaultBackendStorage) *DefaultBackendManager {
	return &DefaultBackendManager{DefaultBackendStorage: bs}
}

// NewDestIPBackendManager returns a DestIPBackendManager
func NewDestIPBackendManager(bs BackendStorage) *DestIPBackendManager {
	return &DestIPBackendManager{BackendStorage: bs}
}

// NewDefaultBackendStorage returns a DefaultBackendStorage
func NewDefaultBackendStorage() *DefaultBackendStorage {
	return &DefaultBackendStorage{
		backends: make(map[string][]*backend),
		random:   rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// GetBackend gets a backend associating to the agentID
func (s *DefaultBackendStorage) GetBackend(agentID string) Backend {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.backends) == 0 {
		return nil
	}
	return s.backends[agentID][0]
}

// AddBackend adds a backend.
func (s *DefaultBackendStorage) AddBackend(agentID string, conn agent.AgentService_ConnectServer) Backend {
	klog.Infof("register Backend %v for agentID %s", conn, agentID)
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.backends[agentID]
	addedBackend := newBackend(conn)
	if ok {
		for _, v := range s.backends[agentID] {
			if v.conn == conn {
				klog.Warningf("this should not happen. Adding existing connection %v for agentID %s", conn, agentID)
				return v
			}
		}
		s.backends[agentID] = append(s.backends[agentID], addedBackend)
		return addedBackend
	}
	s.backends[agentID] = []*backend{addedBackend}
	s.agentIDs = append(s.agentIDs, agentID)
	return addedBackend
}

// RemoveBackend removes a backend.
func (s *DefaultBackendStorage) RemoveBackend(agentID string, conn agent.AgentService_ConnectServer) {
	klog.Infof("remove Backend %v for agentID %s", conn, agentID)
	s.mu.Lock()
	defer s.mu.Unlock()
	backends, ok := s.backends[agentID]
	if !ok {
		klog.Warningf("can't find agentID %s in the backends", agentID)
		return
	}
	var found bool
	for i, c := range backends {
		if c.conn == conn {
			s.backends[agentID] = append(s.backends[agentID][:i], s.backends[agentID][i+1:]...)
			if i == 0 && len(s.backends[agentID]) != 0 {
				klog.Warningf("this should not happen. Removed connection %v that is not the first connection, remaining connections are %v", conn, s.backends[agentID])
			}
			found = true
		}
	}
	if len(s.backends[agentID]) == 0 {
		delete(s.backends, agentID)
		for i := range s.agentIDs {
			if s.agentIDs[i] == agentID {
				s.agentIDs[i] = s.agentIDs[len(s.agentIDs)-1]
				s.agentIDs = s.agentIDs[:len(s.agentIDs)-1]
				break
			}
		}
	}
	if !found {
		klog.Errorf("can't find conn %v for agentID %s in the backends", conn, agentID)
	}
}

// NumBackends resturns the number of available backends
func (s *DefaultBackendStorage) NumBackends() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.backends)
}

// ErrNotFound indicates that no backend can be found.
type ErrNotFound struct{}

// Error returns the error message.
func (e *ErrNotFound) Error() string {
	return "No backend available"
}

// GetRandomBackend returns a random backend.
func (s *DefaultBackendStorage) GetRandomBackend() Backend {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.backends) == 0 {
		return nil
	}
	agentID := s.agentIDs[s.random.Intn(len(s.agentIDs))]
	klog.Infof("pick agentID=%s as backend", agentID)
	// always return the first connection to an agent, because the agent
	// will close later connections if there are multiple.
	return s.backends[agentID][0]
}
