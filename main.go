package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	ds "github.com/ipfs/go-datastore"
	dsync "github.com/ipfs/go-datastore/sync"
	"github.com/ipfs/go-log/v2"
	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	drouting "github.com/libp2p/go-libp2p/p2p/discovery/routing"
	dutil "github.com/libp2p/go-libp2p/p2p/discovery/util"
	rhost "github.com/libp2p/go-libp2p/p2p/host/routed"
	maddr "github.com/multiformats/go-multiaddr"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"
)

// DiscoveryServiceTag is used in our mDNS advertisements to discover other chat peers.
// 通过下面的tag来发现同等节点
const DiscoveryServiceTag = "zyw-nbsd-example"
const periodicIHAVE = 10
const peerLocate = false

var logger = log.Logger("rendezvous")

type addrList []maddr.Multiaddr

func (al *addrList) String() string {
	strs := make([]string, len(*al))
	for i, addr := range *al {
		strs[i] = addr.String()
	}
	return strings.Join(strs, ",")
}

func (al *addrList) Set(value string) error {
	addr, err := maddr.NewMultiaddr(value)
	if err != nil {
		return err
	}
	*al = append(*al, addr)
	return nil
}

func main() {
	var ListenAddresses addrList
	flag.Var(&ListenAddresses, "listen", "Adds a multiaddress to the listen list")
	// create a new libp2p Host that listens on a random TCP port

	host, err := libp2p.New(libp2p.ListenAddrs([]maddr.Multiaddr(ListenAddresses)...),
		//libp2p.EnableNATService(),
		libp2p.EnableRelay(),
	)
	if err != nil {
		panic(err)
	}

	fmt.Printf("\nID:    %s\nAddrs: %s\n\n", host.ID(), host.Addrs())
	// Set a function as stream handler. This function is called when a peer
	// initiates a connection and starts a stream with this peer.
	//host.SetStreamHandler(protocol.ID(host.ID()),)
	ctx := context.Background()
	//Make a datastore used by the DHT
	dstore := dsync.MutexWrap(ds.NewMapDatastore())

	kaddht := dht.NewDHT(ctx, host, dstore)

	//Make the routed host
	routedHost := rhost.Wrap(host, kaddht)

	//Bootstrap the DHT
	if err = bootstrapConnect(ctx, routedHost, dht.GetDefaultBootstrapPeerAddrInfos()); err != nil {
		panic(err)
	}
	if err = kaddht.Bootstrap(ctx); err != nil {
		panic(err)
	}

	//Advertise location
	routingDiscovery := drouting.NewRoutingDiscovery(kaddht)
	dutil.Advertise(ctx, routingDiscovery, DiscoveryServiceTag)
	connectedPeers := make([]peer.ID, 128)

	//Find peers in loop and store them in the peerstore
	go func() {
		for {
			peerChan, err := routingDiscovery.FindPeers(ctx, DiscoveryServiceTag)
			if err != nil {
				panic(err)
			}
			for p := range peerChan {
				if p.ID == routedHost.ID() || containsID(connectedPeers, p.ID) {
					continue
				}
				//Store addresses in the peerstore and connect to the peer found (and localize it)
				if len(p.Addrs) > 0 {
					routedHost.Peerstore().AddAddrs(p.ID, p.Addrs, peerstore.ConnectedAddrTTL)
					err := routedHost.Connect(ctx, p)
					if err == nil {
						logger.Warn("- Connected to", p)
						connectedPeers = append(connectedPeers, p.ID)
					} else {
						fmt.Printf("- Error connecting to %s: %s\n", p, err)
					}
				}

				//logger.Warn(connectedPeers)
			}
		}
	}()
	//Set stream handler to send and receive direct DATA and IWANT messages using the network struct

	// create a new PubSub service using the GossipSub router
	ps, err := pubsub.NewGossipSub(ctx, host)
	if err != nil {
		panic(err)
	}

	//join the chat room
	cr, err := JoinNetwork(ctx, routedHost, ps, routedHost.ID())
	if err != nil {
		panic(err)
	}

	//Periodically send IHAVE messages on the network and locate IPs in the routing table
	go locateIPAddr(kaddht, routedHost)
	go periodicSendIHAVE(cr)

	//Set stream handler to send and receive direct DATA and IWANT messages using the network struct
	routedHost.SetStreamHandler(DiscoveryServiceTag, cr.handleStream)
	go cr.heartBeat()
	//draw the UI
	//ui := NewChatUI(cr)
	//if err = ui.Run(); err != nil {
	//	printErr("error running text UI: %s", err)
	//}

	//Wait for stop signal (Ctrl-C), unsubscribe and close the host
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	go func() {
		<-stop
		for _, conn := range routedHost.Network().Conns() {
			for _, stream := range conn.GetStreams() {
				stream.Reset()
				stream.CloseRead()
				stream.CloseWrite()
			}
		}
		cr.sub.Cancel()
		err = host.Close()
		if err != nil {
			logger.Debug(err)
		}
		err = routedHost.Close()
		if err != nil {
			logger.Debug(err)
		}
		err = kaddht.Close()
		if err != nil {
			logger.Debug(err)
		}
		fmt.Println("Exiting...")
		os.Exit(0)
	}()

	select {}
}

// Bootstrap and connect to the peers
func bootstrapConnect(ctx context.Context, h host.Host, peers []peer.AddrInfo) error {

	//Asynchronously connect to the bootstrap peers
	if len(peers) < 1 {
		return errors.New("not enough bootstrap peers")
	}
	errs := make(chan error, len(peers))
	var wg sync.WaitGroup
	for _, p := range peers {
		wg.Add(1)
		go func(p peer.AddrInfo) {
			defer wg.Done()
			h.Peerstore().AddAddrs(p.ID, p.Addrs, peerstore.ConnectedAddrTTL)
			if err := h.Connect(ctx, p); err != nil {
				logger.Warn("- Error connecting to %s:%s\n", p, err)
				errs <- err
				return
			} else {
				logger.Warn("- Connected to", p)
			}
		}(p)
	}
	wg.Wait()

	//Return errors counting the results
	close(errs)
	count := 0
	var err error
	for err = range errs {
		if err != nil {
			count++
		}
	}
	if count == len(peers) {
		return fmt.Errorf("failed to boostrap: %s", err)
	}
	return nil

}

// Periodically send IHAVE messages on the network for newly entered peers
func periodicSendIHAVE(net *Network) {
	for {
		time.Sleep(time.Second * periodicIHAVE)
		peers := net.ps.ListPeers(DiscoveryServiceTag)
		fmt.Printf("- Found %d other peers in the network: %s\n", len(peers), peers)
		message := &Message{
			MsgType: 2,
			Sender:  net.self,
			IHAVE:   []string{},
			IWANT:   net.host.Peerstore().PeerInfo(net.self),
		}
		err := net.Publish(*message)
		if err != nil {
			fmt.Println(err)
		}
	}
}

// Locate ip addresses stored in the peerstore in loop
func locateIPAddr(kaddht *dht.IpfsDHT, host *rhost.RoutedHost) {
	if !peerLocate {
		logger.Warn("- Peer localitazion disabled")
		return
	}
	for {
		//For each peer in the routing table, find its address in the peerstore and locate it
		for _, p := range kaddht.RoutingTable().ListPeers() {
			listip := make([]string, 0)
			//Analyze every address of the peer found
			for _, addr := range host.Peerstore().PeerInfo(p).Addrs {
				//Locate only new ip4 addresses
				if addr.Protocols()[0].Name == "ip4" {
					ip := strings.Split(addr.String(), "/")[2]
					//Ignore loopback address
					if ip != "127.0.0.1" {
						if !containsIP(listip, ip) {
							listip = append(listip, ip)
						}

					}
				}
			}
		}
		time.Sleep(time.Second * 120)
	}

}

// Check if a string is already in
func containsIP(list []string, ip string) bool {
	for _, found := range list {
		if found == ip {
			return true
		}
	}
	return false
}

// Check if a peer id is in the list given
func containsID(list []peer.ID, p peer.ID) bool {
	for _, pid := range list {
		if pid == p {
			return true
		}
	}
	return false
}
