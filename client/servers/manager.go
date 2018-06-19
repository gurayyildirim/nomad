// Package servers provides an interface for choosing Servers to communicate
// with from a Nomad Client perspective.  The package does not provide any API
// guarantees and should be called only by `hashicorp/nomad`.
package servers

import (
	"context"
	"log"
	"math/rand"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/consul/lib"
)

const (
	// clientRPCMinReuseDuration controls the minimum amount of time RPC
	// queries are sent over an established connection to a single server
	clientRPCMinReuseDuration = 5 * time.Minute

	// Limit the number of new connections a server receives per second
	// for connection rebalancing.  This limit caps the load caused by
	// continual rebalancing efforts when a cluster is in equilibrium.  A
	// lower value comes at the cost of increased recovery time after a
	// partition.  This parameter begins to take effect when there are
	// more than ~48K clients querying 5x servers or at lower server
	// values when there is a partition.
	//
	// For example, in a 100K Nomad cluster with 5x servers, it will
	// take ~5min for all servers to rebalance their connections.  If
	// 99,995 agents are in the minority talking to only one server, it
	// will take ~26min for all servers to rebalance.  A 10K cluster in
	// the same scenario will take ~2.6min to rebalance.
	newRebalanceConnsPerSecPerServer = 64
)

// Pinger is an interface for pinging a server to see if it is healthy.
type Pinger interface {
	Ping(addr net.Addr) error
}

// Server contains the address of a server and metadata that can be used for
// choosing a server to contact.
type Server struct {
	// Addr is the resolved address of the server
	Addr net.Addr
	addr string
	sync.Mutex

	// DC is the datacenter of the server
	DC string
}

func (s *Server) Copy() *Server {
	s.Lock()
	defer s.Unlock()

	return &Server{
		Addr: s.Addr,
		addr: s.addr,
		DC:   s.DC,
	}
}

func (s *Server) String() string {
	s.Lock()
	defer s.Unlock()

	if s.addr == "" {
		s.addr = s.Addr.String()
	}

	return s.addr
}

func (s *Server) Equal(o *Server) bool {
	if s == nil && o == nil {
		return true
	} else if s == nil && o != nil || s != nil && o == nil {
		return false
	}

	return s.Addr.String() == o.Addr.String() && s.DC == o.DC
}

type Servers []*Server

func (s Servers) String() string {
	addrs := make([]string, 0, len(s))
	for _, srv := range s {
		addrs = append(addrs, srv.String())
	}
	return strings.Join(addrs, ",")
}

// cycle cycles a list of servers in-place
func (s Servers) cycle() {
	numServers := len(s)
	if numServers < 2 {
		return // No action required
	}

	start := s[0]
	for i := 1; i < numServers; i++ {
		s[i-1] = s[i]
	}
	s[numServers-1] = start
}

// shuffle shuffles the server list in place
func (s Servers) shuffle() {
	for i := len(s) - 1; i > 0; i-- {
		j := rand.Int31n(int32(i + 1))
		s[i], s[j] = s[j], s[i]
	}
}

func (s Servers) Sort() {
	sort.Slice(s, func(i, j int) bool {
		a, b := s[i], s[j]
		if addr1, addr2 := a.Addr.String(), b.Addr.String(); addr1 == addr2 {
			return a.DC < b.DC
		} else {
			return addr1 < addr2
		}
	})
}

// Equal returns if the two server lists are equal, including the ordering.
func (s Servers) Equal(o Servers) bool {
	if len(s) != len(o) {
		return false
	}

	for i, v := range s {
		if !v.Equal(o[i]) {
			return false
		}
	}

	return true
}

type Manager struct {
	// servers is the list of all known Nomad servers.
	servers Servers

	// rebalanceTimer controls the duration of the rebalance interval
	rebalanceTimer *time.Timer

	//shutdownCh is closed when the parent context is cancelled
	shutdownCh <-chan struct{}

	logger *log.Logger

	// numNodes is used to estimate the approximate number of nodes in
	// a cluster and limit the rate at which it rebalances server
	// connections. This should be read and set using atomic.
	numNodes int32

	// connPoolPinger is used to test the health of a server in the connection
	// pool. Pinger is an interface that wraps client.ConnPool.
	connPoolPinger Pinger

	sync.Mutex
}

// New is the only way to safely create a new Manager struct.
func New(ctx context.Context, logger *log.Logger, connPoolPinger Pinger) (m *Manager) {
	return &Manager{
		logger:         logger,
		connPoolPinger: connPoolPinger,
		rebalanceTimer: time.NewTimer(clientRPCMinReuseDuration),
		shutdownCh:     ctx.Done(),
	}
}

// Start is used to start and manage the task of automatically shuffling and
// rebalancing the list of Nomad servers in order to distribute load across
// all known and available Nomad servers.
func (m *Manager) Start() {
	for {
		select {
		case <-m.rebalanceTimer.C:
			m.RebalanceServers()
			m.refreshServerRebalanceTimer()

		case <-m.shutdownCh:
			m.logger.Printf("[DEBUG] manager: shutting down")
			return
		}
	}
}

// SetServers sets the servers and returns if the new server list is different
// than the existing server set
func (m *Manager) SetServers(servers Servers) bool {
	m.Lock()
	defer m.Unlock()

	// Sort both the  existing and incoming servers
	servers.Sort()
	m.servers.Sort()

	// Determine if they are equal
	equal := servers.Equal(m.servers)

	// Randomize the incoming servers
	servers.shuffle()
	m.servers = servers

	return !equal
}

// FindServer returns a server to send an RPC too. If there are no servers, nil
// is returned.
func (m *Manager) FindServer() *Server {
	m.Lock()
	defer m.Unlock()

	if len(m.servers) == 0 {
		m.logger.Printf("[WARN] manager: No servers available")
		return nil
	}

	// Return whatever is at the front of the list because it is
	// assumed to be the oldest in the server list (unless -
	// hypothetically - the server list was rotated right after a
	// server was added).
	return m.servers[0]
}

// NumNodes returns the number of approximate nodes in the cluster.
func (m *Manager) NumNodes() int32 {
	m.Lock()
	defer m.Unlock()
	return m.numNodes
}

// SetNumNodes stores the number of approximate nodes in the cluster.
func (m *Manager) SetNumNodes(n int32) {
	m.Lock()
	defer m.Unlock()
	m.numNodes = n
}

// NotifyFailedServer marks the passed in server as "failed" by rotating it
// to the end of the server list.
func (m *Manager) NotifyFailedServer(s *Server) {
	m.Lock()
	defer m.Unlock()

	// If the server being failed is not the first server on the list,
	// this is a noop.  If, however, the server is failed and first on
	// the list, move the server to the end of the list.
	if len(m.servers) > 1 && m.servers[0].Equal(s) {
		m.servers.cycle()
	}
}

// NumServers returns the total number of known servers whether healthy or not.
func (m *Manager) NumServers() int {
	m.Lock()
	defer m.Unlock()
	return len(m.servers)
}

// GetServers returns a copy of the current list of servers.
func (m *Manager) GetServers() Servers {
	m.Lock()
	defer m.Unlock()

	copy := make([]*Server, 0, len(m.servers))
	for _, s := range m.servers {
		copy = append(copy, s.Copy())
	}

	return copy
}

// RebalanceServers shuffles the order in which Servers will be contacted. The
// function will shuffle the set of potential servers to contact and then attempt
// to contact each server. If a server successfully responds it is used, otherwise
// it is rotated such that it will be the last attempted server.
func (m *Manager) RebalanceServers() {
	// Shuffle servers so we have a chance of picking a new one.
	servers := m.GetServers()
	servers.shuffle()

	// Iterate through the shuffled server list to find an assumed
	// healthy server.  NOTE: Do not iterate on the list directly because
	// this loop mutates the server list in-place.
	var foundHealthyServer bool
	for i := 0; i < len(m.servers); i++ {
		// Always test the first server.  Failed servers are cycled
		// while Serf detects the node has failed.
		srv := servers[0]

		err := m.connPoolPinger.Ping(srv.Addr)
		if err == nil {
			foundHealthyServer = true
			break
		}
		m.logger.Printf(`[DEBUG] manager: pinging server "%s" failed: %s`, srv, err)

		servers.cycle()
	}

	if !foundHealthyServer {
		m.logger.Printf("[DEBUG] manager: No healthy servers during rebalance")
		return
	}

	// Save the servers
	m.Lock()
	m.servers = servers
	m.Unlock()
}

// refreshServerRebalanceTimer is only called once m.rebalanceTimer expires.
func (m *Manager) refreshServerRebalanceTimer() time.Duration {
	m.Lock()
	defer m.Unlock()
	numServers := len(m.servers)

	// Limit this connection's life based on the size (and health) of the
	// cluster.  Never rebalance a connection more frequently than
	// connReuseLowWatermarkDuration, and make sure we never exceed
	// clusterWideRebalanceConnsPerSec operations/s across numLANMembers.
	clusterWideRebalanceConnsPerSec := float64(numServers * newRebalanceConnsPerSecPerServer)

	connRebalanceTimeout := lib.RateScaledInterval(clusterWideRebalanceConnsPerSec, clientRPCMinReuseDuration, int(m.numNodes))
	connRebalanceTimeout += lib.RandomStagger(connRebalanceTimeout)

	m.rebalanceTimer.Reset(connRebalanceTimeout)
	return connRebalanceTimeout
}

// ResetRebalanceTimer resets the rebalance timer.  This method exists for
// testing and should not be used directly.
func (m *Manager) ResetRebalanceTimer() {
	m.Lock()
	defer m.Unlock()
	m.rebalanceTimer.Reset(clientRPCMinReuseDuration)
}
