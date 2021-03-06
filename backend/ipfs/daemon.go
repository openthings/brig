package ipfs

import (
	"errors"
	"fmt"
	"net"
	"os"
	"sync"

	log "github.com/Sirupsen/logrus"

	core "github.com/ipfs/go-ipfs/core"
	fsrepo "github.com/ipfs/go-ipfs/repo/fsrepo"

	"golang.org/x/net/context"
)

// Find the next free tcp port near to `port` (possibly euqal to `port`).
// Only `maxTries` number of trials will be made.
// This method is (of course...) racy since the port might be already
// taken again by another process until we startup our service on that port.
func findFreePortAfter(port int, maxTries int) int {
	for idx := 0; idx < maxTries; idx++ {
		addr := fmt.Sprintf("localhost:%d", port+idx)
		lst, err := net.Listen("tcp", addr)
		if err != nil {
			continue
		}

		if err := lst.Close(); err != nil {
			// TODO: Well? Maybe do something?
		}

		return port + idx
	}

	return port
}

var (
	// ErrIsOffline is returned when an online operation was done offline.
	ErrIsOffline = errors.New("Node is offline")
)

// Node remembers the settings needed for accessing the ipfs daemon.
type Node struct {
	Path      string
	SwarmPort int

	mu sync.Mutex

	ipfsNode *core.IpfsNode

	// Root context used for all operations.
	ctx    context.Context
	cancel context.CancelFunc
}

func createNode(path string, swarmPort int, ctx context.Context, online bool) (*core.IpfsNode, error) {

	if _, err := os.Stat(path); os.IsNotExist(err) {
		log.Infof("Creating new ipfs repo at %s since it does not exist yet.", path)
		if err := Init(path, 2048); err != nil {
			return nil, err
		}
	}

	rp, err := fsrepo.Open(path)
	if err != nil {
		log.Errorf("Unable to open repo `%s`: %v", path, err)
		return nil, err
	}

	swarmPort = findFreePortAfter(4002, 100)

	log.Debugf(
		"ipfs node configured to run on swarm port %d",
		swarmPort,
	)

	config := map[string]interface{}{
		"Addresses.Swarm": []string{
			fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", swarmPort),
			fmt.Sprintf("/ip6/::/tcp/%d", swarmPort),
		},
		"Addresses.API":     "", // fmt.Sprintf("/ip4/127.0.0.1/tcp/%d", apiPort),
		"Addresses.Gateway": "", // fmt.Sprintf("/ip4/127.0.0.1/tcp/%d", gatewayPort),
	}

	for key, value := range config {
		if err := rp.SetConfigKey(key, value); err != nil {
			return nil, err
		}
	}

	cfg := &core.BuildCfg{
		Repo:   rp,
		Online: online,
	}

	ipfsNode, err := core.NewNode(ctx, cfg)
	if err != nil {
		return nil, err
	}

	return ipfsNode, nil
}

// New creates a new ipfs node manager.
// No daemon is started yet.
func New(ipfsPath string) (*Node, error) {
	return NewWithPort(ipfsPath, 4001)
}

func NewWithPort(ipfsPath string, swarmPort int) (*Node, error) {
	ctx, cancel := context.WithCancel(context.Background())
	ipfsNode, err := createNode(ipfsPath, swarmPort, ctx, true)
	if err != nil {
		return nil, err
	}

	return &Node{
		Path:      ipfsPath,
		SwarmPort: swarmPort,
		ipfsNode:  ipfsNode,
		ctx:       ctx,
		cancel:    cancel,
	}, nil
}

func (nd *Node) IsOnline() bool {
	nd.mu.Lock()
	defer nd.mu.Unlock()

	return nd.isOnline()
}

func (nd *Node) isOnline() bool {
	return nd.ipfsNode.OnlineMode()
}

func (nd *Node) Connect() error {
	nd.mu.Lock()
	defer nd.mu.Unlock()

	if nd.isOnline() {
		return nil
	}

	var err error
	nd.ipfsNode, err = createNode(nd.Path, nd.SwarmPort, nd.ctx, true)
	if err != nil {
		return err
	}

	return nil
}

func (nd *Node) Disconnect() error {
	nd.mu.Lock()
	defer nd.mu.Unlock()

	if !nd.isOnline() {
		return ErrIsOffline
	}

	var err error
	nd.ipfsNode, err = createNode(nd.Path, nd.SwarmPort, nd.ctx, false)
	if err != nil {
		return err
	}

	return nil
}

// Close shuts down the ipfs node.
// It may not be used afterwards.
func (nd *Node) Close() error {
	nd.cancel()
	return nd.ipfsNode.Close()
}

func (nd *Node) Name() string {
	return "ipfs"
}
