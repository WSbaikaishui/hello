package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"

	"log"
	"time"
)

type Network struct {
	// Messages is a channel of messages received from other peers in the chat room
	Messages chan *Message

	ctx   context.Context
	ps    *pubsub.PubSub
	host  host.Host
	topic *pubsub.Topic
	sub   *pubsub.Subscription
	self  peer.ID
}

type Message struct {
	MsgType int //0 Heartbeat packet, 1 heartOk 2 advertiseSelf
	Sender  peer.ID
	IHAVE   []string
	IWANT   peer.AddrInfo
}

// Join the GossipSub network
func JoinNetwork(ctx context.Context, host host.Host, ps *pubsub.PubSub, self peer.ID) (*Network, error) {

	//Join the topic
	topic, err := ps.Join(DiscoveryServiceTag)
	if err != nil {
		return nil, err
	}

	//Subscribe to the topic
	sub, err := topic.Subscribe()
	if err != nil {
		return nil, err
	}

	net := &Network{
		ctx:      ctx,
		host:     host,
		ps:       ps,
		topic:    topic,
		sub:      sub,
		self:     self,
		Messages: make(chan *Message, 1024),
	}

	log.Println("- Network joined successfully, topic:", topic.String())

	go net.ReadService()
	return net, nil

}

// Loop service for GossipSub that handles IHAVE messages
func (net *Network) ReadService() {
	for {

		//Get next message in the topic
		received, err := net.sub.Next(net.ctx)
		if err != nil {
			close(net.Messages)
			return
		}

		//If I'm the sender, ignore the message
		if received.ReceivedFrom == net.self {
			logger.Info("- I am the sender, ignoring the packet")
			continue
		}

		//Unmarshal the message
		message := new(Message)
		err = json.Unmarshal(received.Data, message)
		if err != nil {
			continue
		}
		fmt.Println(message)
	}
}

// Publishes the message on the GossipSub network (used only for IHAVE messages)
func (net *Network) Publish(message Message) error {
	msg, err := json.Marshal(message)
	if err != nil {
		return err
	}
	//Publish the message on the GossipSub network
	err = net.topic.Publish(net.ctx, msg)
	if err != nil {
		return err
	}
	logger.Warn("- Message IHAVE published (%d chars): %s\n", len(msg), string(msg))
	return nil
}

// Send directly from a peer to another
func (net *Network) directSend(receiver peer.ID, msg Message) {

	//Open the stream to the receiver peer
	stream, err := net.host.NewStream(net.ctx, receiver, DiscoveryServiceTag)
	if err != nil {
		fmt.Printf("- Error opening stream to %s: %s\n", receiver, err)
		return
	}

	//Marshal the message to send it
	message, err := json.Marshal(msg)
	if err != nil {
		fmt.Println("- Error marshalling message to send:", err)
		return
	}

	//Write the message on the stream
	_, err = stream.Write(message)
	if err != nil {
		fmt.Println("- Error sending message on stream:", err)
		return
	}

}

// Stream handler for DATA and IWANT messages
func (net *Network) handleStream(s network.Stream) {

	rw := bufio.NewReadWriter(bufio.NewReader(s), bufio.NewWriter(s))

	//Receive and respond message loop
	go func(rw *bufio.ReadWriter) {
		//Create the decoder for the stream
		decoder := json.NewDecoder(rw)
		var message Message
		for {

			//Get the message and decode it
			err := decoder.Decode(&message)

			if err != nil {
				log.Println("- Stream closed:", err)
				s.Reset()
				return
			}
			switch message.MsgType {
			case 0:
				msg := &Message{
					MsgType: 1,
					Sender:  net.self,
					IHAVE:   []string{"ok"},
					IWANT:   peer.AddrInfo{},
				}
				net.directSend(message.Sender, *msg)
			case 1:
				fmt.Println(message)
				continue
			}

		}
	}(rw)

}

func (net *Network) heartBeat() {
	for {
		time.Sleep(time.Second * periodicIHAVE)
		peers := net.ps.ListPeers(DiscoveryServiceTag)
		for _, listpeer := range peers {
			msg := &Message{
				MsgType: 0,
				Sender:  net.self,
				IHAVE:   []string{},
				IWANT:   peer.AddrInfo{},
			}

			net.directSend(listpeer, *msg)
		}
	}
}
