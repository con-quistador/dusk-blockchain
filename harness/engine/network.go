// This Source Code Form is subject to the terms of the MIT License.
// If a copy of the MIT License was not distributed with this
// file, you can obtain one at https://opensource.org/licenses/MIT.
//
// Copyright (c) DUSK NETWORK. All rights reserved.

package engine

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/dusk-network/dusk-blockchain/pkg/config"
	"github.com/dusk-network/dusk-blockchain/pkg/rpc/client"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"google.golang.org/grpc"
)

var (
	// EnableHarness a test CLI param to enable harness bootstrapping.
	EnableHarness = flag.Bool("enable", false, "Enable Test Harness bootstrapping")
	// RPCNetworkType a test CLI param to set jsonrpc network type (unix or tcp).
	RPCNetworkType = flag.String("rpc_transport", "unix", "JSON-RPC transport type (unix/tcp)")
	// KeepAlive a test CLI param to keep harness running even after all tests have passed.
	// It's useful when additional manual tests should be done.
	KeepAlive = flag.Bool("keepalive", false, "Keep Test Harness alive after tests pass")

	// ErrDisabledHarness yields a disabled test harness.
	ErrDisabledHarness = errors.New("disabled test harness")

	// MOCK_ADDRESS is optional string for the mock address to listen to, eg: 127.0.0.1:8080.
	MOCK_ADDRESS = os.Getenv("MOCK_ADDRESS")

	// REQUIRE_SESSION is a flag to set the GRPC session.
	REQUIRE_SESSION = os.Getenv("REQUIRE_SESSION")

	// RUSK_EXE_PATH path to rusk executable.
	RUSK_EXE_PATH = os.Getenv("RUSK_PATH")
)

const yes = "true"

// GrpcClient is an interface that abstracts the way to connect to the grpc
// server (i.e. with or without a session).
type GrpcClient interface {
	// GetSessionConn returns a connection to the grpc server.
	GetSessionConn(options ...grpc.DialOption) (*grpc.ClientConn, error)
	// GracefulClose closes the connection.
	GracefulClose(options ...grpc.DialOption)
}

type sessionlessClient struct {
	network string
	addr    string
	conn    *grpc.ClientConn
}

// GetSessionConn returns a connection to the grpc server.
func (s *sessionlessClient) GetSessionConn(opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	addr := s.addr

	if s.network == "unix" { //nolint
		addr = "unix://" + addr
	}

	var cancel context.CancelFunc
	var dialCtx context.Context

	dialCtx, cancel = context.WithTimeout(context.Background(),
		5*time.Second)
	defer cancel()

	// Set up a connection to the server.
	return grpc.DialContext(dialCtx, addr, opts...)
}

// GracefulClose closes the connection.
func (s *sessionlessClient) GracefulClose(options ...grpc.DialOption) {
	defer func() {
		if r := recover(); r != nil {
			log.Println("recovered error in closing the connection", r)
		}
	}()

	if s != nil && s.conn != nil {
		_ = s.conn.Close()
	}
}

const (
	// GossipNetwork Gossip network type.
	GossipNetwork = byte(0)
	// KadcastNetwork  Kadcast network type.
	KadcastNetwork = byte(1)
)

// Network describes the current network configuration in terms of nodes and
// processes.
type Network struct {
	grpcClients map[string]GrpcClient
	nodes       []*DuskNode
	processes   []*os.Process

	NetworkType byte
}

// AddNode to the network.
func (n *Network) AddNode(node *DuskNode) {
	n.nodes = append(n.nodes, node)
}

// AddGrpcClient creates the right grpc client linked to the node through
// the Id of the latter.
func (n *Network) AddGrpcClient(nodeID, network, addr string) {
	if n.grpcClients == nil {
		n.grpcClients = make(map[string]GrpcClient)
	}
	var c GrpcClient

	if n.IsSessionRequired() {
		c = client.New(network, addr)
	} else {
		c = &sessionlessClient{network: network, addr: addr}
	}

	n.grpcClients[nodeID] = c
}

// Size of the network intended as nunber of nodes.
func (n *Network) Size() int {
	return len(n.nodes)
}

// Bootstrap performs all actions needed to initialize and start a local network.
// This network is alive by the end of all tests execution.
func (n *Network) Bootstrap(workspace string) error {
	// Network bootstrapping is disabled by default as it's intended to be run
	// on demand only but not by CI for now.
	// To enable it: go test -v ./...  -args -enable
	if !*EnableHarness {
		log.Println("Test Harness bootstrapping is disabled.")
		log.Println("To enable it: `go test -v ./...  -args -enable`")
		return ErrDisabledHarness
	}

	initProfiles()

	_, _, seederExec, err := n.getExec()
	if err != nil {
		return err
	}

	// Start voucher seeder if gossip network is selected.
	if n.NetworkType == GossipNetwork {
		if len(seederExec) > 0 {
			if err := n.start(workspace, seederExec); err != nil {
				return err
			}
		} else {
			// If path not provided, then it's assumed that the seeder is already running.
			log.Warnf("Seeder path not provided. Please, ensure dusk-seeder is already running")
		}
	}

	// Foreach node read localNet.Nodes, configure and run new nodes
	for i, node := range n.nodes {
		if err := n.StartNode(i, node, workspace); err != nil {
			return err
		}

		// avoid stressing dusk-seeder
		time.Sleep(time.Duration(1) * time.Second)
	}

	log.Infof("Local network workspace: %s", workspace)
	log.Infof("Running %d nodes", len(n.nodes))

	// Allow network nodes to complete their startup procedures
	delay := 2 * len(n.nodes)
	if delay > 20 {
		delay = 20
	}

	time.Sleep(time.Duration(delay) * time.Second)
	return nil
}

// IsSessionRequired returns whether a session is required or otherwise.
func (n *Network) IsSessionRequired() bool {
	return REQUIRE_SESSION == yes
}

func (n *Network) closeGRPCConnections() {
	var wg sync.WaitGroup

	for _, grpcC := range n.grpcClients {
		wg.Add(1)

		c := grpcC
		go func(cli GrpcClient) {
			cli.GracefulClose(grpc.WithInsecure())
			wg.Done()
		}(c)
	}

	wg.Wait()
}

// Teardown the network.
func (n *Network) Teardown() {
	n.closeGRPCConnections()

	for _, p := range n.processes {
		if err := p.Signal(os.Interrupt); err != nil {
			log.Warn(err)
		}
	}
}

// StartNode locally.
//nolint
func (n *Network) StartNode(i int, node *DuskNode, workspace string) error {
	blockchainExec, utilsExec, _, err := n.getExec()
	if err != nil {
		return err
	}

	// create node folder
	nodeDir := workspace + "/node-" + node.Id
	if e := os.Mkdir(nodeDir, os.ModeDir|os.ModePerm); e != nil {
		return e
	}

	node.Dir = nodeDir

	// Load wallet path as walletX.dat are hard-coded for now
	// Later they could be generated on the fly per each test execution
	walletsPath, _ := os.Getwd()
	walletsPath += "/../../devnet-wallets/"

	// Generate node default config file
	tomlFilePath, tomlErr := n.generateConfig(i, walletsPath)
	if tomlErr != nil {
		return tomlErr
	}

	if MOCK_ADDRESS != "" {
		// Start the mock RUSK server
		if startErr := n.start(nodeDir, utilsExec, "mockrusk",
			"--rusknetwork", node.Cfg.RPC.Rusk.Network,
			"--ruskaddress", node.Cfg.RPC.Rusk.Address,
			"--walletstore", node.Cfg.Wallet.Store,
			"--walletfile", node.Cfg.Wallet.File,
			"--configfile", tomlFilePath,
			// Optional "--cpuprofile", nodeDir+"/mockrusk_cpu.prof",
		); startErr != nil {
			return startErr
		}
	}

	// Run Network service (Kadcast server)
	// NB. Both Rusk Mock and Rusk executable are in use until we fully integrate Rusk State service.
	if n.NetworkType == KadcastNetwork {
		cfg := node.Cfg.Kadcast
		switch cfg.Grpc.Network {
		case "tcp":
			addr, port, err := net.SplitHostPort(cfg.Grpc.Address)
			if err != nil {
				panic(err)
			}

			if err := n.startRusk(nodeDir, cfg.BootstrapAddr, cfg.Address, addr, port); err != nil {
				return err
			}

		case "unix":
			if err := n.startRuskWithUDS(nodeDir, cfg.BootstrapAddr, cfg.Address, cfg.Grpc.Address); err != nil {
				return err
			}
		default:
			panic("unsupported network type")
		}
	}

	// Run dusk-blockchain node process
	if startErr := n.start(nodeDir, blockchainExec, "--config", tomlFilePath); startErr != nil {
		return startErr
	}

	n.AddGrpcClient(node.Id, node.Cfg.RPC.Network, node.Cfg.RPC.Address)
	return nil
}

// GetGrpcConn gets a connection to the GRPC server of a node. It delegates
// eventual sessions to the underlying client.
func (n *Network) GetGrpcConn(i uint, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	c := n.grpcClients[n.nodes[i].Id]
	return c.GetSessionConn(opts...)
}

// generateConfig loads config profile assigned to the node identified by an
// index.
// It's based on viper global var so it cannot be called concurrently.
func (n *Network) generateConfig(nodeIndex int, walletPath string) (string, error) {
	node := n.nodes[nodeIndex]

	// Load config profile from the global parameter profileList
	profileFunc, ok := profileList[node.ConfigProfileID]
	if !ok {
		return "", fmt.Errorf("invalid config profile for node index %d", nodeIndex)
	}

	// profileFunc mutates the configuration for a node, so inject the
	// parameters which depend on its sandbox
	profileFunc(nodeIndex, node, walletPath)

	// setting the root directory for node's sandbox
	configPath := node.Dir + "/dusk.toml"
	if err := viper.WriteConfigAs(configPath); err != nil {
		return "", fmt.Errorf("config profile err '%s' for node index %d", err.Error(), nodeIndex)
	}

	// Finally load sandbox configuration and setting it in the node
	var err error

	node.Cfg, err = config.LoadFromFile(configPath)
	if err != nil {
		return "", fmt.Errorf("LoadFromFile %s failed with err %s", configPath, err.Error())
	}

	return configPath, nil
}

// Start an OS process with TMPDIR=nodeDir, manageable by the network.
func (n *Network) start(nodeDir string, name string, arg ...string) error {
	//nolint:gosec
	cmd := exec.Command(name, arg...)
	cmd.Env = os.Environ()
	cmd.Env = append(cmd.Env, "TMPDIR="+nodeDir)

	// Redirect both STDOUT and STDERR to separate files
	if len(nodeDir) > 0 {
		id := filepath.Base(name)

		stdOutFile, err := os.Create(nodeDir + "/" + id + "_stdout")
		if err != nil {
			log.Panic(err)
		}

		var stdErrFile *os.File

		stdErrFile, err = os.Create(nodeDir + "/" + id + "_stderr")
		if err != nil {
			log.Panic(err)
		}

		cmd.Stdout = stdOutFile
		cmd.Stderr = stdErrFile
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	n.processes = append(n.processes, cmd.Process)
	return nil
}

// getExec returns paths of all node executables.
// dusk-blockchain, blindbid and seeder.
func (n *Network) getExec() (string, string, string, error) {
	blockchainExec, err := getEnv("DUSK_BLOCKCHAIN")
	if err != nil {
		return "", "", "", err
	}

	utilsExec, err := getEnv("DUSK_UTILS")
	if err != nil {
		return "", "", "", err
	}

	seederExec, err := getEnv("DUSK_SEEDER")
	if err != nil {
		return "", "", "", err
	}

	return blockchainExec, utilsExec, seederExec, nil
}

func getEnv(envVarName string) (string, error) {
	execPath := os.Getenv(envVarName)
	if len(execPath) == 0 {
		return "", fmt.Errorf("ENV variable %s is not declared", envVarName)
	}

	if _, err := os.Stat(execPath); os.IsNotExist(err) {
		return "", fmt.Errorf("ENV variable %s points at non-existing file", envVarName)
	}

	return execPath, nil
}

func (n *Network) startRusk(nodeDir string, bootstrapNodes []string, kadcastPublicAddr, grpcAddr, grpcPort string) error {
	if err := n.start(nodeDir, RUSK_EXE_PATH,
		"--ipc_method", "tcp_ip",
		"--host", grpcAddr,
		"--port", grpcPort,
		"--kadcast_public_address", kadcastPublicAddr,
		"--kadcast_bootstrap", bootstrapNodes[0],
		"--kadcast_bootstrap", bootstrapNodes[1],
		"--log-level", "info",
	); err != nil {
		return err
	}

	return nil
}

func (n *Network) startRuskWithUDS(nodeDir string, bootstrapNodes []string, kadcastPublicAddr string, grpcSocket string) error {
	if err := n.start(nodeDir, RUSK_EXE_PATH,
		"--ipc_method", "uds",
		"--socket", grpcSocket,
		"--kadcast_public_address", kadcastPublicAddr,
		"--kadcast_bootstrap", bootstrapNodes[0],
		"--kadcast_bootstrap", bootstrapNodes[1],
		"--log-level", "info",
	); err != nil {
		return err
	}

	return nil
}
