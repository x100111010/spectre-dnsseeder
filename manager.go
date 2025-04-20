// Copyright (c) 2018 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/spectre-project/spectred/infrastructure/network/addressmanager"

	"github.com/miekg/dns"
	"github.com/pkg/errors"
	"github.com/spectre-project/spectred/app/appmessage"
	"github.com/spectre-project/spectred/domain/consensus/model/externalapi"
)

// Node repesents a node in the Spectre network
type Node struct {
	Addr         *appmessage.NetAddress
	UserAgent    *string
	LastAttempt  time.Time
	LastSuccess  time.Time
	LastSeen     time.Time
	SubnetworkID *externalapi.DomainSubnetworkID
}

// Manager is dnsseeder's main worker-type, storing all information required
// for operation
type Manager struct {
	mtx sync.RWMutex

	nodes     map[string]*Node
	wg        sync.WaitGroup
	quit      chan struct{}
	peersFile string
}

const (
	// defaultMaxAddresses is the maximum number of addresses to return.
	defaultMaxAddresses = 16

	// defaultStaleGoodTimeout is the time in which a previously reachable
	// node is considered stale.
	defaultStaleGoodTimeout = time.Hour

	// defaultStaleBadTimeout is the time in which a previously unreachable
	// node is considered stale.
	defaultStaleBadTimeout = time.Hour * 2

	// dumpAddressInterval is the interval used to dump the address
	// cache to disk for future use.
	dumpAddressInterval = time.Minute * 2

	// peersFilename is the name of the file.
	peersFilename = "nodes.json"

	// pruneAddressInterval is the interval used to run the address
	// pruner.
	pruneAddressInterval = time.Minute * 1

	// pruneExpireTimeout is the expire time in which a node is
	// considered dead.
	pruneExpireTimeout = time.Hour * 8
)

// NewManager constructs and returns a new dnsseeder manager, with the provided dataDir
func NewManager(dataDir string) (*Manager, error) {
	amgr := Manager{
		nodes:     make(map[string]*Node),
		peersFile: filepath.Join(dataDir, peersFilename),
		quit:      make(chan struct{}),
	}

	err := amgr.deserializePeers()
	if err != nil {
		log.Warnf("Failed to parse file %s: %v", amgr.peersFile, err)
		// if it is invalid we nuke the old one unconditionally.
		err = os.Remove(amgr.peersFile)
		if err != nil {
			log.Warnf("Failed to remove corrupt peers file %s: %v",
				amgr.peersFile, err)
		}
	}

	amgr.wg.Add(1)
	spawn("NewManager-Manager.addressHandler", amgr.addressHandler)

	return &amgr, nil
}

// AddAddresses adds an address to this dnsseeder manager, and returns the number of
// address currently held
func (m *Manager) AddAddresses(addrs []*appmessage.NetAddress) int {
	var count int

	m.mtx.Lock()
	for _, addr := range addrs {
		if addr.Port == 0 || !addressmanager.IsRoutable(addr, ActiveConfig().NetParams().AcceptUnroutable) {
			continue
		}
		addrStr := addr.IP.String() + "_" + strconv.Itoa(int(addr.Port))

		_, exists := m.nodes[addrStr]
		if exists {
			m.nodes[addrStr].LastSeen = time.Now()
			continue
		}
		node := Node{
			Addr:     addr,
			LastSeen: time.Now(),
		}
		m.nodes[addrStr] = &node
		count++
	}
	m.mtx.Unlock()

	return count
}

// Addresses returns IPs that need to be tested again.
func (m *Manager) Addresses() []*appmessage.NetAddress {
	addrs := make([]*appmessage.NetAddress, 0, 2000)
	i := ActiveConfig().Threads * 3

	m.mtx.RLock()
	for _, node := range m.nodes {
		if i == 0 {
			break
		}
		if !isStale(node) {
			continue
		}
		addrs = append(addrs, node.Addr)
		i--
	}
	m.mtx.RUnlock()

	return addrs
}

// AddressCount returns number of known nodes.
func (m *Manager) AddressCount() int {
	return len(m.nodes)
}

// GoodAddresses returns good working IPs that match both the
// passed DNS query type and have the requested services.
func (m *Manager) GoodAddresses(qtype uint16, includeAllSubnetworks bool, subnetworkID *externalapi.DomainSubnetworkID,
) []*appmessage.NetAddress {
	addrs := make([]*appmessage.NetAddress, 0, defaultMaxAddresses)
	i := defaultMaxAddresses

	if qtype != dns.TypeA && qtype != dns.TypeAAAA {
		return addrs
	}

	m.mtx.RLock()
	for _, node := range m.nodes {
		if i == 0 {
			break
		}
		if !includeAllSubnetworks && !node.SubnetworkID.Equal(subnetworkID) {
			continue
		}
		if qtype == dns.TypeA && !isIPv4(node.Addr) ||
			qtype == dns.TypeAAAA && isIPv4(node.Addr) {
			continue
		}
		if !isGood(node) {
			continue
		}
		addrs = append(addrs, node.Addr)
		i--
	}
	m.mtx.RUnlock()

	return addrs
}

// Attempt updates the last connection attempt for the specified ip address to now
func (m *Manager) Attempt(addr *appmessage.NetAddress) {
	m.mtx.Lock()
	node, exists := m.nodes[addr.IP.String()+"_"+strconv.Itoa(int(addr.Port))]
	if exists {
		node.LastAttempt = time.Now()
	}
	m.mtx.Unlock()
}

// Good updates the last successful connection attempt for the specified ip address to now
func (m *Manager) Good(addr *appmessage.NetAddress, userAgent *string, subnetworkid *externalapi.DomainSubnetworkID) {
	m.mtx.Lock()
	node, exists := m.nodes[addr.IP.String()+"_"+strconv.Itoa(int(addr.Port))]
	if exists {
		node.UserAgent = userAgent
		node.LastSuccess = time.Now()
		node.SubnetworkID = subnetworkid
	}
	m.mtx.Unlock()
}

// addressHandler is the main handler for the address manager. It must be run
// as a goroutine.
func (m *Manager) addressHandler() {
	defer m.wg.Done()
	pruneAddressTicker := time.NewTicker(pruneAddressInterval)
	defer pruneAddressTicker.Stop()
	dumpAddressTicker := time.NewTicker(dumpAddressInterval)
	defer dumpAddressTicker.Stop()
out:
	for {
		select {
		case <-dumpAddressTicker.C:
			m.savePeers()
		case <-pruneAddressTicker.C:
			m.prunePeers()
		case <-m.quit:
			break out
		}
	}
	log.Infof("Address manager: saving peers")
	m.savePeers()
	log.Infof("Address manager shutdown")
}

func (m *Manager) prunePeers() {
	var pruned, good, stale, bad, ipv4, ipv6 int
	m.mtx.Lock()

	for k, node := range m.nodes {
		if isExpired(node) {
			delete(m.nodes, k)
			pruned++
		} else if isGood(node) {
			good++
			if isIPv4(node.Addr) {
				ipv4++
			} else {
				ipv6++
			}
		} else if isStale(node) {
			stale++
		} else {
			bad++
		}
	}
	total := len(m.nodes)
	m.mtx.Unlock()

	log.Infof("Pruned %d addresses. %d left.", pruned, total)
	log.Infof("Known nodes: Good:%d [4:%d, 6:%d] Stale:%d Bad:%d", good, ipv4, ipv6, stale, bad)
}

func (m *Manager) deserializePeers() error {
	filePath := m.peersFile
	_, err := os.Stat(filePath)
	if os.IsNotExist(err) {
		return nil
	}
	r, err := os.Open(filePath)
	if err != nil {
		return errors.Errorf("%s error opening file: %v", filePath, err)
	}
	defer r.Close()

	var nodes map[string]*Node
	dec := json.NewDecoder(r)
	err = dec.Decode(&nodes)
	if err != nil {
		return errors.Errorf("error reading %s: %v", filePath, err)
	}

	l := len(nodes)

	m.mtx.Lock()
	m.nodes = nodes
	m.mtx.Unlock()

	log.Infof("%d nodes loaded", l)
	return nil
}

func (m *Manager) savePeers() {
	m.mtx.RLock()
	defer m.mtx.RUnlock()

	// Write temporary peers file and then move it into place.
	tmpfile := m.peersFile + ".new"
	w, err := os.Create(tmpfile)
	if err != nil {
		log.Errorf("Error opening file %s: %v", tmpfile, err)
		return
	}
	enc := json.NewEncoder(w)
	if err := enc.Encode(&m.nodes); err != nil {
		log.Errorf("Failed to encode file %s: %v", tmpfile, err)
		return
	}
	if err := w.Close(); err != nil {
		log.Errorf("Error closing file %s: %v", tmpfile, err)
		return
	}
	if err := os.Rename(tmpfile, m.peersFile); err != nil {
		log.Errorf("Error writing file %s: %v", m.peersFile, err)
		return
	}
}

func isGood(node *Node) bool {
	return !isNonDefaultPort(node.Addr) && time.Since(node.LastSuccess) < defaultStaleGoodTimeout
}

func isStale(node *Node) bool {
	return !node.LastSuccess.IsZero() && time.Since(node.LastAttempt) > defaultStaleGoodTimeout ||
		time.Since(node.LastAttempt) > defaultStaleBadTimeout
}

func isExpired(node *Node) bool {
	return time.Since(node.LastSeen) > pruneExpireTimeout &&
		time.Since(node.LastSuccess) > pruneExpireTimeout
}

func isNonDefaultPort(addr *appmessage.NetAddress) bool {
	return addr.Port != uint16(peersDefaultPort)
}

func isIPv4(addr *appmessage.NetAddress) bool {
	return addr.IP.To4() != nil
}
