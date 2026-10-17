// Copyright 2021 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package bridge_test

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/refs"
	"gvisor.dev/gvisor/pkg/state"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/faketime"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/link/veth"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

func TestWritePacketFromBridge(t *testing.T) {
	const (
		channelLinkAddr1 = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x05")
		channelLinkAddr2 = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x06")
		remoteLinkAddr   = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x07")
		bridgeLinkAddr   = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x08")

		netProto = 55
		nicID1   = 5
		nicID2   = 6
		bridgeID = 7
	)

	// Create two channel-based endpoints as receivers, they are both
	// bound to the same bridge device and are both expected to receive the
	// packets that are written by the bridge.
	ch1 := channel.New(1, header.EthernetMinimumSize, channelLinkAddr1)
	ch2 := channel.New(1, header.EthernetMinimumSize, channelLinkAddr2)
	bridgeEndpoint := stack.NewBridgeEndpoint(1500)
	bridgeEndpoint.SetLinkAddress(bridgeLinkAddr)
	s := stack.New(stack.Options{})

	if err := s.CreateNIC(nicID1, ethernet.New(ch1)); err != nil {
		t.Fatalf("s.CreateNIC(%d, _): %s", nicID1, err)
	}
	if err := s.CreateNIC(nicID2, ethernet.New(ch2)); err != nil {
		t.Fatalf("s.CreateNIC(%d, _): %s", nicID2, err)
	}
	if err := s.CreateNIC(bridgeID, bridgeEndpoint); err != nil {
		t.Fatalf("s.CreateNIC(%d, _): %s", bridgeID, err)
	}
	if err := s.SetNICCoordinator(nicID1, bridgeID); err != nil {
		t.Fatalf("s.SetNICCoordinator(%d, %d)", nicID1, bridgeID)
	}
	if err := s.SetNICCoordinator(nicID2, bridgeID); err != nil {
		t.Fatalf("s.SetNICCoordinator(%d, %d)", nicID2, bridgeID)
	}
	// When writing packets, the bridge will try all available bridge ports.
	if err := s.WritePacketToRemote(bridgeID, remoteLinkAddr, netProto, buffer.Buffer{}); err != nil {
		t.Fatalf("s.WritePacketToRemote(%d, %s, _): %s", bridgeID, remoteLinkAddr, err)
	}
	for _, c := range []*channel.Endpoint{ch1, ch2} {
		pkt := c.Read()
		if pkt == nil {
			t.Fatal("expected to read a packet")
		}

		eth := header.Ethernet(pkt.LinkHeader().Slice())
		pkt.DecRef()
		if got := eth.SourceAddress(); got != bridgeLinkAddr {
			t.Errorf("got eth.SourceAddress() = %s, want = %s", got, bridgeLinkAddr)
		}
		if got := eth.DestinationAddress(); got != remoteLinkAddr {
			t.Errorf("got eth.DestinationAddress() = %s, want = %s", got, remoteLinkAddr)
		}
		if got := eth.Type(); got != netProto {
			t.Errorf("got eth.Type() = %d, want = %d", got, netProto)
		}
	}
}

type testNotification struct {
	ch chan bool
}

func (n *testNotification) WriteNotify() {
	n.ch <- true
}

// The test verifies that packates that are forwarded by
// a bridge will flooded to all bridge ports.
func TestWritePacketBetweenDevices(t *testing.T) {
	const (
		channelLinkAddr1 = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x04")
		channelLinkAddr2 = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x05")
		vethLinkAddr1    = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x06")
		vethLinkAddr2    = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x07")
		remoteLinkAddr   = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x08")
		bridgeLinkAddr   = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x09")

		netProto = 55
		nicID1   = 4
		nicID2   = 5
		vethID   = 6
		bridgeID = 9
	)
	// Creates a pair of veth devices which will be attached to different
	// network stacks.
	veth1, veth2 := veth.NewPair(1500, veth.DefaultBacklogSize)
	veth1.SetLinkAddress(vethLinkAddr1)
	veth2.SetLinkAddress(vethLinkAddr2)
	ch1 := channel.New(1, header.EthernetMinimumSize, channelLinkAddr1)
	ch2 := channel.New(1, header.EthernetMinimumSize, channelLinkAddr2)

	bridgeEndpoint := stack.NewBridgeEndpoint(1500)
	bridgeEndpoint.SetLinkAddress(bridgeLinkAddr)
	s := stack.New(stack.Options{})
	secondStack := stack.New(stack.Options{})
	if err := s.CreateNIC(bridgeID, bridgeEndpoint); err != nil {
		t.Fatalf("s.CreateNIC(%d, _): %s", bridgeID, err)
	}
	if err := s.CreateNIC(nicID1, ethernet.New(ch1)); err != nil {
		t.Fatalf("s.CreateNIC(%d, _): %s", nicID1, err)
	}
	if err := s.SetNICCoordinator(nicID1, bridgeID); err != nil {
		t.Fatalf("s.SetNICCoordinator(%d, %d)", nicID1, bridgeID)
	}
	if err := s.CreateNIC(nicID2, ethernet.New(ch2)); err != nil {
		t.Fatalf("s.CreateNIC(%d, _): %s", nicID2, err)
	}
	if err := s.SetNICCoordinator(nicID2, bridgeID); err != nil {
		t.Fatalf("s.SetNICCoordinator(%d, %d)", nicID2, bridgeID)
	}
	if err := s.CreateNIC(vethID, ethernet.New(veth1)); err != nil {
		t.Fatalf("s.CreateNIC(%d, _): %s", vethID, err)
	}
	// Attach one veth device to stack s, and attach the other
	// veth to stack secondStack.
	if err := s.SetNICCoordinator(vethID, bridgeID); err != nil {
		t.Fatalf("s.SetNICCoordinator")
	}
	if err := secondStack.CreateNIC(vethID, ethernet.New(veth2)); err != nil {
		t.Fatalf("s.CreateNIC(%d, _): %s", vethID, err)
	}

	n1 := &testNotification{ch: make(chan bool, 1)}
	n2 := &testNotification{ch: make(chan bool, 1)}
	ch1.AddNotify(n1)
	ch2.AddNotify(n2)
	// Write a packet to the veth device at the stack secondStack, the packet
	// will be available at the veth device at the stack s.
	if err := secondStack.WritePacketToRemote(vethID, remoteLinkAddr, netProto, buffer.Buffer{}); err != nil {
		t.Fatalf("s.WritePacketToRemote(%d, %s, _): %s", bridgeID, remoteLinkAddr, err)
	}
	<-n1.ch
	<-n2.ch
	// No FDB entry, a package floods all bridge ports except the port that
	// is attached to the veth device.
	for _, c := range []*channel.Endpoint{ch1, ch2} {
		pkt := c.Read()
		if pkt == nil {
			t.Fatal("expected to read a packet")
		}

		pkt.LinkHeader().Consume(header.EthernetMinimumSize)
		eth := header.Ethernet(pkt.LinkHeader().Slice())
		pkt.DecRef()
		if got := eth.SourceAddress(); got != vethLinkAddr2 {
			t.Errorf("got eth.SourceAddress() = %s, want = %s", got, vethLinkAddr2)
		}
		if got := eth.DestinationAddress(); got != remoteLinkAddr {
			t.Errorf("got eth.DestinationAddress() = %s, want = %s", got, remoteLinkAddr)
		}
		if got := eth.Type(); got != netProto {
			t.Errorf("got eth.Type() = %d, want = %d", got, netProto)
		}
	}
}

func TestBridgeFDB(t *testing.T) {
	const (
		channelLinkAddr = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x03")
		remoteLinkAddr  = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x08")
		bridgeLinkAddr  = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x09")

		netProto = 55
		nicID    = 4
		vethID1  = 7
		vethID2  = 8
		bridgeID = 9
	)
	veth1, veth2 := veth.NewPair(1500, veth.DefaultBacklogSize)
	ch := channel.New(1, header.EthernetMinimumSize, channelLinkAddr)
	bridgeEndpoint := stack.NewBridgeEndpoint(1500)
	bridgeEndpoint.SetLinkAddress(bridgeLinkAddr)
	s := stack.New(stack.Options{})
	secondStack := stack.New(stack.Options{})

	if err := s.CreateNIC(bridgeID, bridgeEndpoint); err != nil {
		t.Fatalf("s.CreateNIC(%d, _): %s", bridgeID, err)
	}
	if err := s.CreateNIC(nicID, ethernet.New(ch)); err != nil {
		t.Fatalf("s.CreateNIC(%d, _): %s", nicID, err)
	}
	if err := s.SetNICCoordinator(nicID, bridgeID); err != nil {
		t.Fatalf("s.SetNICCoordinator(%d, %d)", nicID, bridgeID)
	}
	if err := s.CreateNIC(vethID1, ethernet.New(veth1)); err != nil {
		t.Fatalf("s.CreateNIC(%d, _): %s", vethID1, err)
	}
	if err := s.SetNICCoordinator(vethID1, bridgeID); err != nil {
		t.Fatalf("s.SetNICCoordinator(%d, %d)", vethID1, bridgeID)
	}
	if err := secondStack.CreateNIC(vethID2, ethernet.New(veth2)); err != nil {
		t.Fatalf("s.CreateNIC(%d, _): %s", vethID2, err)
	}
	n := &testNotification{ch: make(chan bool, 1)}
	ch.AddNotify(n)
	// Write a packet to the veth device at secondStack, the
	// packet will be available at stack s via veth.
	if err := secondStack.WritePacketToRemote(vethID2, remoteLinkAddr, netProto, buffer.Buffer{}); err != nil {
		t.Fatalf("s.WritePacketToRemote(%d, %s, _): %s", bridgeID, remoteLinkAddr, err)
	}
	<-n.ch
	pkt := ch.Read()
	defer pkt.DecRef()
	if pkt == nil {
		t.Fatal("expected to read a packet")
	}
	// When forwarding the packet via the bridge device, the packet's
	// source MAC address will be used as lookup key to the bridge
	// FDB.
	var e stack.BridgeFDBEntry
	start := time.Now()
	for {
		e = bridgeEndpoint.FindFDBEntry(veth2.LinkAddress())
		if len(e.PortLinkAddress()) != 0 {
			break
		}
		if time.Since(start) > 30*time.Second {
			t.Fatalf("failed to find FDB entry for %s after 30 seconds", veth2.LinkAddress())
		}
		time.Sleep(time.Second)
	}
	if e.PortLinkAddress() != veth1.LinkAddress() {
		t.Fatalf("bridgeEndpoint.FindFDBEntry(%s) = %s, want = %s", veth2.LinkAddress(), e.PortLinkAddress(), veth1.LinkAddress())
	}
	// No FDB entry is expected for devices other than veth1.
	if e := bridgeEndpoint.FindFDBEntry(channelLinkAddr); len(e.PortLinkAddress()) != 0 {
		t.Fatalf("bridgeEndpoint.FindFDBEntry(%s) = %s, want = \"\"", channelLinkAddr, e.PortLinkAddress())
	}
}

func TestSetCoordinator(t *testing.T) {
	const (
		bridgeLinkAddr = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x08")
		bridgeID       = 6
	)

	s := stack.New(stack.Options{})
	bridgeEndpoint := stack.NewBridgeEndpoint(1500)
	bridgeEndpoint.SetLinkAddress(bridgeLinkAddr)
	if err := s.CreateNIC(bridgeID, bridgeEndpoint); err != nil {
		t.Fatalf("s.CreateNIC(%d, _): %s", bridgeID, err)
	}
	if err := s.SetNICCoordinator(bridgeID, bridgeID); err == nil {
		t.Fatalf("s.SetNICCoordinator(%d, %d) = %s, want = %s", bridgeID, bridgeID, err, tcpip.ErrNoSuchFile{})
	}
}

func TestMTU(t *testing.T) {
	e := stack.NewBridgeEndpoint(1500)
	mtus := []uint32{1000, 2000}
	for _, mtu := range mtus {
		e.SetMTU(mtu)

		if want, v := mtu-header.EthernetMinimumSize, e.MTU(); want != v {
			t.Errorf("MTU() = %v, want %v", v, want)
		}
	}
}

type testPacketEndpoint struct {
	count int
}

func (p *testPacketEndpoint) HandlePacket(tcpip.NICID, tcpip.NetworkProtocolNumber, *stack.PacketBuffer) {
	p.count++
}

func TestNicExitingStackExitsBridgeToo(t *testing.T) {
	const (
		netProto        = 55
		nicID           = 5
		bridgeID        = 7
		bridgeLinkAddr  = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x09")
		channelLinkAddr = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x04")
		remoteLinkAddr  = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x07")
	)

	src := stack.New(stack.Options{})
	defer src.Destroy()

	// Create a bridge in src.
	bridgeEndpoint := stack.NewBridgeEndpoint(1500)
	bridgeEndpoint.SetLinkAddress(bridgeLinkAddr)
	if err := src.CreateNIC(bridgeID, bridgeEndpoint); err != nil {
		t.Fatalf("s.CreateNIC(%d, _): %s", bridgeID, err)
	}

	// Create a channel-endpoint-based nic in src with non-default options so we
	// also verify SetNICStack preserves Context, DeliverLinkPackets, and
	// EnableExperimentIPOption.
	const wantContext = "port-nic-context"
	ch := channel.New(2, header.EthernetMinimumSize, channelLinkAddr)
	if err := src.CreateNICWithOptions(nicID, ethernet.New(ch), stack.NICOptions{
		Name:                     "port0",
		Context:                  wantContext,
		DeliverLinkPackets:       true,
		EnableExperimentIPOption: true,
	}); err != nil {
		t.Fatalf("src.CreateNICWithOptions(%d, _): %s", nicID, err)
	}
	// And add it to the bridge.
	if err := src.SetNICCoordinator(nicID, bridgeID); err != nil {
		t.Fatalf("s.SetNICCoordinator(%d, %d)", nicID, bridgeID)
	}

	// The bridge should forward pkts to the nic.
	if err := src.WritePacketToRemote(bridgeID, remoteLinkAddr, netProto, buffer.Buffer{}); err != nil {
		t.Fatalf("s.WritePacketToRemote(%d, %s, _): %s", bridgeID, remoteLinkAddr, err)
	}
	pkt := ch.Read()
	if pkt == nil {
		t.Fatal("expected to read a packet")
	}
	pkt.DecRef()

	// Now move the nic into dst.
	dst := stack.New(stack.Options{})
	defer dst.Destroy()
	dstNICID, err := src.SetNICStack(nicID, dst)
	if err != nil {
		t.Fatalf("s.SetNICStack(%d, %p) = %s, want nil", nicID, dst, err)
	}

	// Verify Context, EnableExperimentIPOption, and DeliverLinkPackets were
	// preserved on the moved NIC in dst.
	info, ok := dst.SingleNICInfo(dstNICID)
	if !ok {
		t.Fatalf("dst.SingleNICInfo(%d) = (_, false), want true", dstNICID)
	}
	if info.Context != wantContext {
		t.Errorf("dst.SingleNICInfo(%d).Context = %v, want %q", dstNICID, info.Context, wantContext)
	}
	dstNIC, nicErr := dst.GetNICByID(dstNICID)
	if nicErr != nil {
		t.Fatalf("dst.GetNICByID(%d) = (_, %s), want nil", dstNICID, nicErr)
	}
	if !dstNIC.GetExperimentIPOptionEnabled() {
		t.Errorf("dstNIC.GetExperimentIPOptionEnabled() = false, want true")
	}
	var pktEP testPacketEndpoint
	if err := dst.RegisterPacketEndpoint(dstNICID, header.EthernetProtocolAll, &pktEP); err != nil {
		t.Fatalf("dst.RegisterPacketEndpoint(%d, _): %s", dstNICID, err)
	}
	defer dst.UnregisterPacketEndpoint(dstNICID, header.EthernetProtocolAll, &pktEP)
	if err := dst.WritePacketToRemote(dstNICID, remoteLinkAddr, netProto, buffer.Buffer{}); err != nil {
		t.Fatalf("dst.WritePacketToRemote(%d, %s, _): %s", dstNICID, remoteLinkAddr, err)
	}
	if pkt := ch.Read(); pkt != nil {
		pkt.DecRef()
	}
	if pktEP.count != 1 {
		t.Errorf("pktEP.count = %d, want 1 (DeliverLinkPackets preserved)", pktEP.count)
	}

	// The bridge should no longer forward pkts to the nic, lest it defeat the purpose of network
	// namespaces.
	if err := src.WritePacketToRemote(bridgeID, remoteLinkAddr, netProto, buffer.Buffer{}); err != nil {
		t.Fatalf("s.WritePacketToRemote(%d, %s, _): %s", bridgeID, remoteLinkAddr, err)
	}
	pkt = ch.Read()
	if pkt != nil {
		pkt.DecRef()
		t.Fatal("did not expect to read a packet")
	}
}

// makeEthernetFrame builds an inbound frame whose ethernet header is still in
// the payload, for injection into a channel endpoint wrapped by ethernet.New.
func makeEthernetFrame(src, dst tcpip.LinkAddress) *stack.PacketBuffer {
	hdr := make([]byte, header.EthernetMinimumSize)
	header.Ethernet(hdr).Encode(&header.EthernetFields{
		SrcAddr: src,
		DstAddr: dst,
		Type:    header.IPv4ProtocolNumber,
	})
	return stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: buffer.MakeWithData(hdr),
	})
}

// drainChannels releases every packet queued on the given endpoints.
func drainChannels(eps ...*channel.Endpoint) {
	for _, ep := range eps {
		for pkt := ep.Read(); pkt != nil; pkt = ep.Read() {
			pkt.DecRef()
		}
	}
}

// TestBridgeDispatchOnlyForLocalOrMulticast checks that only group frames and
// frames addressed to the bridge or to the receiving port are passed up; other
// frames are only forwarded.
func TestBridgeDispatchOnlyForLocalOrMulticast(t *testing.T) {
	const (
		channelLinkAddr1 = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x10")
		channelLinkAddr2 = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x11")
		bridgeLinkAddr   = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x12")
		unknownLinkAddr  = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x13")

		nicID1   = 11
		nicID2   = 12
		bridgeID = 13
	)

	tests := []struct {
		name       string
		dst        tcpip.LinkAddress
		wantUpcall bool
		// checkFwd checks that port 2 receives the frame. Frames for the bridge
		// or the receiving port are not checked because they are still flooded.
		checkFwd bool
	}{
		{name: "unicast to other port", dst: channelLinkAddr2, checkFwd: true},
		{name: "unknown unicast", dst: unknownLinkAddr, checkFwd: true},
		{name: "unicast to bridge", dst: bridgeLinkAddr, wantUpcall: true},
		{name: "unicast to receiving port", dst: channelLinkAddr1, wantUpcall: true},
		{name: "broadcast", dst: header.EthernetBroadcastAddress, wantUpcall: true, checkFwd: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ch1 := channel.New(1, header.EthernetMinimumSize, channelLinkAddr1)
			ch2 := channel.New(1, header.EthernetMinimumSize, channelLinkAddr2)
			bridgeEndpoint := stack.NewBridgeEndpoint(1500)
			bridgeEndpoint.SetLinkAddress(bridgeLinkAddr)

			s := stack.New(stack.Options{})
			defer s.Destroy()
			if err := s.CreateNIC(bridgeID, bridgeEndpoint); err != nil {
				t.Fatalf("s.CreateNIC(%d, _): %s", bridgeID, err)
			}
			if err := s.CreateNIC(nicID1, ethernet.New(ch1)); err != nil {
				t.Fatalf("s.CreateNIC(%d, _): %s", nicID1, err)
			}
			if err := s.CreateNIC(nicID2, ethernet.New(ch2)); err != nil {
				t.Fatalf("s.CreateNIC(%d, _): %s", nicID2, err)
			}
			if err := s.SetNICCoordinator(nicID1, bridgeID); err != nil {
				t.Fatalf("s.SetNICCoordinator(%d, %d): %s", nicID1, bridgeID, err)
			}
			if err := s.SetNICCoordinator(nicID2, bridgeID); err != nil {
				t.Fatalf("s.SetNICCoordinator(%d, %d): %s", nicID2, bridgeID, err)
			}

			// Learn port 2 so that frames to it are forwarded, not flooded.
			learn := makeEthernetFrame(channelLinkAddr2, channelLinkAddr1)
			ch2.InjectInbound(header.IPv4ProtocolNumber, learn)
			learn.DecRef()
			drainChannels(ch1, ch2)

			rx := s.NICInfo()[bridgeID].Stats.Rx.Packets
			before := rx.Value()
			pkt := makeEthernetFrame(channelLinkAddr1, test.dst)
			ch1.InjectInbound(header.IPv4ProtocolNumber, pkt)
			pkt.DecRef()

			var wantUpcalls uint64
			if test.wantUpcall {
				wantUpcalls = 1
			}
			if got := rx.Value() - before; got != wantUpcalls {
				t.Errorf("got %d frames passed up to the bridge, want %d", got, wantUpcalls)
			}
			if test.checkFwd {
				if fwd := ch2.Read(); fwd == nil {
					t.Error("frame was not forwarded to port 2")
				} else {
					fwd.LinkHeader().Consume(header.EthernetMinimumSize)
					if got := header.Ethernet(fwd.LinkHeader().Slice()).DestinationAddress(); got != test.dst {
						t.Errorf("got forwarded destination = %s, want = %s", got, test.dst)
					}
					fwd.DecRef()
				}
			}

			drainChannels(ch1, ch2)
			ch1.Close()
			ch2.Close()
		})
	}
}

func TestBridgeFDBSaveRestore(t *testing.T) {
	const (
		channelLinkAddr     = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x03")
		vethLinkAddr1       = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x06")
		vethLinkAddr2       = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x07")
		bridgeLinkAddr      = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x09")
		unpreservedLinkAddr = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x0a")
		unpreservedPeerAddr = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x0b")

		netProto      = 55
		nicID         = 4
		vethID1       = 7
		vethID2       = 8
		bridgeID      = 9
		unpreservedID = 10
	)

	veth1, veth2 := veth.NewPair(1500, veth.DefaultBacklogSize)
	veth1.SetLinkAddress(vethLinkAddr1)
	veth2.SetLinkAddress(vethLinkAddr2)
	ch := channel.New(1, header.EthernetMinimumSize, channelLinkAddr)
	unpreservedCh := channel.New(2, header.EthernetMinimumSize, unpreservedLinkAddr)
	bridgeEndpoint := stack.NewBridgeEndpoint(1500)
	bridgeEndpoint.SetLinkAddress(bridgeLinkAddr)

	s := stack.New(stack.Options{})
	secondStack := stack.New(stack.Options{})

	if err := s.CreateNICWithOptions(bridgeID, bridgeEndpoint, stack.NICOptions{Name: "br0", Kind: stack.NICKindBridge}); err != nil {
		t.Fatalf("s.CreateNICWithOptions(%d, _): %s", bridgeID, err)
	}
	// Mark channel endpoint ch0 with Kind: stack.NICKindVeth so Stack preserves
	// it across save/restore alongside the bridge and real veth port.
	if err := s.CreateNICWithOptions(nicID, ethernet.New(ch), stack.NICOptions{Name: "ch0", Kind: stack.NICKindVeth}); err != nil {
		t.Fatalf("s.CreateNICWithOptions(%d, _): %s", nicID, err)
	}
	if err := s.SetNICCoordinator(nicID, bridgeID); err != nil {
		t.Fatalf("s.SetNICCoordinator(%d, %d): %s", nicID, bridgeID, err)
	}
	if err := s.CreateNICWithOptions(vethID1, ethernet.New(veth1), stack.NICOptions{Name: "veth1", Kind: stack.NICKindVeth}); err != nil {
		t.Fatalf("s.CreateNICWithOptions(%d, _): %s", vethID1, err)
	}
	if err := s.SetNICCoordinator(vethID1, bridgeID); err != nil {
		t.Fatalf("s.SetNICCoordinator(%d, %d): %s", vethID1, bridgeID, err)
	}
	// Enslave an unpreserved NIC (empty Kind, like a host-FD-backed eth0) to
	// verify that a removeConf=false checkpoint omits it from the saved bridge
	// without detaching it from the live bridge.
	if err := s.CreateNICWithOptions(unpreservedID, ethernet.New(unpreservedCh), stack.NICOptions{Name: "eth0"}); err != nil {
		t.Fatalf("s.CreateNICWithOptions(%d, _): %s", unpreservedID, err)
	}
	if err := s.SetNICCoordinator(unpreservedID, bridgeID); err != nil {
		t.Fatalf("s.SetNICCoordinator(%d, %d): %s", unpreservedID, bridgeID, err)
	}
	if err := secondStack.CreateNICWithOptions(vethID2, ethernet.New(veth2), stack.NICOptions{Name: "veth2", Kind: stack.NICKindVeth}); err != nil {
		t.Fatalf("secondStack.CreateNICWithOptions(%d, _): %s", vethID2, err)
	}

	n := &testNotification{ch: make(chan bool, 1)}
	notifyHandle := ch.AddNotify(n)
	if err := secondStack.WritePacketToRemote(vethID2, channelLinkAddr, netProto, buffer.Buffer{}); err != nil {
		t.Fatalf("secondStack.WritePacketToRemote(%d, %s, _): %s", vethID2, channelLinkAddr, err)
	}
	select {
	case <-n.ch:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for packet across veth and bridge before save")
	}
	ch.RemoveNotify(notifyHandle)
	pkt := ch.Read()
	if pkt == nil {
		t.Fatal("ch.Read() = nil before save, want non-nil")
	}
	pkt.DecRef()

	// Also learn channelLinkAddr on port nicID and unpreservedPeerAddr on
	// unpreservedID before saving.
	learn := makeEthernetFrame(channelLinkAddr, vethLinkAddr2)
	ch.InjectInbound(header.IPv4ProtocolNumber, learn)
	learn.DecRef()

	unpreservedLearn := makeEthernetFrame(unpreservedPeerAddr, vethLinkAddr2)
	unpreservedCh.InjectInbound(header.IPv4ProtocolNumber, unpreservedLearn)
	unpreservedLearn.DecRef()

	if got := bridgeEndpoint.FindFDBEntry(vethLinkAddr2).PortLinkAddress(); got != vethLinkAddr1 {
		t.Fatalf("bridgeEndpoint.FindFDBEntry(%s) = %s, want %s", vethLinkAddr2, got, vethLinkAddr1)
	}
	if got := bridgeEndpoint.FindFDBEntry(unpreservedPeerAddr).PortLinkAddress(); got != unpreservedLinkAddr {
		t.Fatalf("bridgeEndpoint.FindFDBEntry(%s) = %s, want %s", unpreservedPeerAddr, got, unpreservedLinkAddr)
	}

	s.SetRemoveConf(false)
	s.Pause()
	secondStack.Pause()
	saved := [2]*stack.Stack{s, secondStack}
	var buf bytes.Buffer
	if _, err := state.Save(t.Context(), &buf, &saved); err != nil {
		t.Fatalf("state.Save failed: %v", err)
	}

	// Saving with removeConf=false must leave the live bridge intact.
	if got := bridgeEndpoint.FindFDBEntry(unpreservedPeerAddr).PortLinkAddress(); got != unpreservedLinkAddr {
		t.Fatalf("live bridgeEndpoint.FindFDBEntry(%s) after save = %s, want %s", unpreservedPeerAddr, got, unpreservedLinkAddr)
	}

	s.Destroy()
	secondStack.Destroy()

	var loaded [2]*stack.Stack
	if _, err := state.Load(t.Context(), bytes.NewReader(buf.Bytes()), &loaded); err != nil {
		t.Fatalf("state.Load failed: %v", err)
	}
	restoredRoot, restoredChild := loaded[0], loaded[1]
	restoredRoot.ResetConfig()
	restoredRoot.Restore()
	restoredChild.Restore()
	defer func() {
		restoredRoot.Destroy()
		restoredChild.Destroy()
	}()

	if restoredRoot.HasNIC(unpreservedID) {
		t.Fatalf("restoredRoot.HasNIC(%d) = true, want false for unpreserved bridge port", unpreservedID)
	}

	restoredBridge, ok := restoredRoot.GetLinkEndpointByName("br0").(*stack.BridgeEndpoint)
	if !ok || restoredBridge == nil {
		t.Fatalf("restoredRoot.GetLinkEndpointByName(\"br0\") = %T, want *stack.BridgeEndpoint", restoredRoot.GetLinkEndpointByName("br0"))
	}
	if got := restoredBridge.FindFDBEntry(vethLinkAddr2).PortLinkAddress(); got != vethLinkAddr1 {
		t.Fatalf("restoredBridge.FindFDBEntry(%s) = %s, want %s", vethLinkAddr2, got, vethLinkAddr1)
	}
	if got := restoredBridge.FindFDBEntry(unpreservedPeerAddr).PortLinkAddress(); got != "" {
		t.Fatalf("restoredBridge.FindFDBEntry(%s) = %s, want empty for unpreserved bridge port", unpreservedPeerAddr, got)
	}

	restoredChEth, ok := restoredRoot.GetLinkEndpointByName("ch0").(*ethernet.Endpoint)
	if !ok {
		t.Fatalf("restoredRoot.GetLinkEndpointByName(\"ch0\") = %T, want *ethernet.Endpoint", restoredRoot.GetLinkEndpointByName("ch0"))
	}
	restoredCh, ok := restoredChEth.Child().(*channel.Endpoint)
	if !ok {
		t.Fatalf("restoredChEth.Child() = %T, want *channel.Endpoint", restoredChEth.Child())
	}

	n2 := &testNotification{ch: make(chan bool, 1)}
	restoredCh.AddNotify(n2)
	if err := restoredChild.WritePacketToRemote(vethID2, channelLinkAddr, netProto, buffer.Buffer{}); err != nil {
		t.Fatalf("restoredChild.WritePacketToRemote(%d, %s, _): %s", vethID2, channelLinkAddr, err)
	}
	select {
	case <-n2.ch:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for packet across restored veth and bridge")
	}
	restoredPkt := restoredCh.Read()
	if restoredPkt == nil {
		t.Fatal("restoredCh.Read() = nil after restore, want non-nil")
	}
	restoredPkt.DecRef()
}

type dadEvent struct {
	nicID tcpip.NICID
	addr  tcpip.Address
	res   stack.DADResult
}

type testNDPDispatcherContextKey struct{}

type testNDPDispatcher struct {
	ipv6.NDPDispatcher
	dadCh chan dadEvent
}

func (*testNDPDispatcher) StateTypeName() string {
	return "gvisor.dev/gvisor/pkg/tcpip/stack/bridge_test.testNDPDispatcher"
}

func (*testNDPDispatcher) StateFields() []string { return nil }

func (*testNDPDispatcher) StateSave(state.Sink) {}

func (d *testNDPDispatcher) StateLoad(ctx context.Context, _ state.Source) {
	*d = *ctx.Value(testNDPDispatcherContextKey{}).(*testNDPDispatcher)
}

func (d *testNDPDispatcher) OnDuplicateAddressDetectionResult(nicID tcpip.NICID, addr tcpip.Address, res stack.DADResult) {
	if d.dadCh != nil {
		d.dadCh <- dadEvent{nicID: nicID, addr: addr, res: res}
	}
}

func init() {
	state.Register((*testNDPDispatcher)(nil))
}

type dstCaptureDispatcher struct {
	ch chan tcpip.Address
}

func (d *dstCaptureDispatcher) DeliverNetworkPacket(protocol tcpip.NetworkProtocolNumber, pkt *stack.PacketBuffer) {
	view := pkt.ToView()
	defer view.Release()
	raw := view.AsSlice()
	if linkLen := len(pkt.LinkHeader().Slice()); len(raw) >= linkLen {
		raw = raw[linkLen:]
	}
	switch protocol {
	case header.IPv4ProtocolNumber:
		if len(raw) >= header.IPv4MinimumSize {
			d.ch <- header.IPv4(raw).DestinationAddress()
		}
	case header.IPv6ProtocolNumber:
		if len(raw) >= header.IPv6MinimumSize {
			d.ch <- header.IPv6(raw).DestinationAddress()
		}
	}
}

func (*dstCaptureDispatcher) DeliverLinkPacket(tcpip.NetworkProtocolNumber, *stack.PacketBuffer) {}

func makeIPv4EthernetFrame(srcMAC, dstMAC tcpip.LinkAddress, srcIP, dstIP tcpip.Address) *stack.PacketBuffer {
	raw := make([]byte, header.EthernetMinimumSize+header.IPv4MinimumSize)
	header.Ethernet(raw[:header.EthernetMinimumSize]).Encode(&header.EthernetFields{
		SrcAddr: srcMAC,
		DstAddr: dstMAC,
		Type:    header.IPv4ProtocolNumber,
	})
	ip := header.IPv4(raw[header.EthernetMinimumSize:])
	ip.Encode(&header.IPv4Fields{
		TotalLength: header.IPv4MinimumSize,
		TTL:         64,
		Protocol:    uint8(header.UDPProtocolNumber),
		SrcAddr:     srcIP,
		DstAddr:     dstIP,
	})
	ip.SetChecksum(^ip.CalculateChecksum())
	return stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: buffer.MakeWithData(raw),
	})
}

func makeIPv6EthernetFrame(srcMAC, dstMAC tcpip.LinkAddress, srcIP, dstIP tcpip.Address) *stack.PacketBuffer {
	raw := make([]byte, header.EthernetMinimumSize+header.IPv6MinimumSize)
	header.Ethernet(raw[:header.EthernetMinimumSize]).Encode(&header.EthernetFields{
		SrcAddr: srcMAC,
		DstAddr: dstMAC,
		Type:    header.IPv6ProtocolNumber,
	})
	ip6 := header.IPv6(raw[header.EthernetMinimumSize:])
	ip6.Encode(&header.IPv6Fields{
		PayloadLength:     0,
		TransportProtocol: header.UDPProtocolNumber,
		HopLimit:          64,
		SrcAddr:           srcIP,
		DstAddr:           dstIP,
	})
	return stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: buffer.MakeWithData(raw),
	})
}

func TestSaveRestoreNICIDAndReplaceConfigCollision(t *testing.T) {
	const (
		eth0MAC     = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x01")
		eth1MAC     = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x02")
		eth0PeerMAC = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x11")
		ext0MAC     = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x21")
		ext0PeerMAC = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x22")
		ext1MAC     = tcpip.LinkAddress("\x02\x02\x03\x04\x05\x23")
	)

	eth0V4 := tcpip.AddressWithPrefix{Address: tcpip.AddrFrom4([4]byte{10, 0, 0, 1}), PrefixLen: 24}
	eth0PeerV4 := tcpip.AddrFrom4([4]byte{10, 0, 0, 2})
	eth0V6 := tcpip.AddressWithPrefix{Address: tcpip.AddrFrom16([16]byte{0x20, 0x01, 0x0d, 0xb8, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}), PrefixLen: 64}
	eth0PeerV6 := tcpip.AddrFrom16([16]byte{0x20, 0x01, 0x0d, 0xb8, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2})

	ext0V4 := tcpip.AddressWithPrefix{Address: tcpip.AddrFrom4([4]byte{192, 168, 1, 1}), PrefixLen: 24}
	ext0PeerV4 := tcpip.AddrFrom4([4]byte{192, 168, 1, 2})
	ext0V6 := tcpip.AddressWithPrefix{Address: tcpip.AddrFrom16([16]byte{0x20, 0x01, 0x0d, 0xb8, 0, 2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}), PrefixLen: 64}
	ext0PeerV6 := tcpip.AddrFrom16([16]byte{0x20, 0x01, 0x0d, 0xb8, 0, 2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2})

	dadConfigs := stack.DADConfigurations{
		DupAddrDetectTransmits: 1,
		RetransmitTimer:        time.Second,
	}
	dadDisp := &testNDPDispatcher{dadCh: make(chan dadEvent, 16)}
	clock := faketime.NewManualClock()
	s := stack.New(stack.Options{
		NetworkProtocols: []stack.NetworkProtocolFactory{
			arp.NewProtocol,
			ipv4.NewProtocol,
			ipv6.NewProtocolWithOptions(ipv6.Options{
				DADConfigs: dadConfigs,
				NDPDisp:    dadDisp,
			}),
		},
		Clock: clock,
	})
	if err := s.SetForwardingDefaultAndAllNICs(ipv4.ProtocolNumber, true); err != nil {
		t.Fatalf("s.SetForwardingDefaultAndAllNICs(ipv4): %s", err)
	}
	if err := s.SetForwardingDefaultAndAllNICs(ipv6.ProtocolNumber, true); err != nil {
		t.Fatalf("s.SetForwardingDefaultAndAllNICs(ipv6): %s", err)
	}

	// Create preserved NICs at IDs 1 and 2.
	ch1 := channel.New(16, 1500, eth0MAC)
	ch2 := channel.New(16, 1500, eth1MAC)
	for i, ch := range []*channel.Endpoint{ch1, ch2} {
		id := s.NextNICID()
		if want := tcpip.NICID(i + 1); id != want {
			t.Fatalf("NextNICID() = %d, want %d", id, want)
		}
		if err := s.CreateNICWithOptions(id, ethernet.New(ch), stack.NICOptions{
			Name: "eth" + string(rune('0'+i)),
			Kind: stack.NICKindVeth,
		}); err != nil {
			t.Fatalf("CreateNICWithOptions(%d): %s", id, err)
		}
	}
	if err := s.AddProtocolAddress(1, tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: eth0V4,
	}, stack.AddressProperties{}); err != nil {
		t.Fatalf("s.AddProtocolAddress(1, ipv4): %s", err)
	}
	if err := s.AddProtocolAddress(1, tcpip.ProtocolAddress{
		Protocol:          ipv6.ProtocolNumber,
		AddressWithPrefix: eth0V6,
	}, stack.AddressProperties{}); err != nil {
		t.Fatalf("s.AddProtocolAddress(1, ipv6): %s", err)
	}
	clock.Advance(time.Second)
	drainChannels(ch1, ch2)
	for len(dadDisp.dadCh) > 0 {
		<-dadDisp.dadCh
	}
	if err := s.AddStaticNeighbor(1, ipv4.ProtocolNumber, eth0PeerV4, eth0PeerMAC); err != nil {
		t.Fatalf("s.AddStaticNeighbor(1, ipv4): %s", err)
	}
	if err := s.AddStaticNeighbor(1, ipv6.ProtocolNumber, eth0PeerV6, eth0PeerMAC); err != nil {
		t.Fatalf("s.AddStaticNeighbor(1, ipv6): %s", err)
	}

	eth0SubnetV4 := eth0V4.Subnet()
	eth0SubnetV6 := eth0V6.Subnet()
	s.SetRouteTable([]tcpip.Route{
		{Destination: eth0SubnetV4, NIC: 1},
		{Destination: eth0SubnetV6, NIC: 1},
	})

	s.Pause()
	var buf bytes.Buffer
	if _, err := state.Save(t.Context(), &buf, s); err != nil {
		t.Fatalf("state.Save failed: %v", err)
	}
	s.Destroy()

	var restored stack.Stack
	loadCtx := context.WithValue(t.Context(), testNDPDispatcherContextKey{}, dadDisp)
	if _, err := state.Load(loadCtx, bytes.NewReader(buf.Bytes()), &restored); err != nil {
		t.Fatalf("state.Load failed: %v", err)
	}
	defer restored.Destroy()

	// Build a replacement config stack with IPv4/IPv6 protocols and gapped NIC
	// IDs {1, 4}, where ID 1 collides with preserved NIC 1 ("eth0"). Use veth
	// pairs for both ext0 and ext1 so we verify onCloseAction rebinding on both
	// colliding and non-colliding ReplaceConfig NICs.
	replacement := stack.New(stack.Options{
		NetworkProtocols: []stack.NetworkProtocolFactory{
			arp.NewProtocol,
			ipv4.NewProtocol,
			ipv6.NewProtocolWithOptions(ipv6.Options{
				DADConfigs: dadConfigs,
			}),
		},
		Clock: faketime.NewManualClock(),
	})
	if err := replacement.SetForwardingDefaultAndAllNICs(ipv4.ProtocolNumber, true); err != nil {
		t.Fatalf("replacement.SetForwardingDefaultAndAllNICs(ipv4): %s", err)
	}
	if err := replacement.SetForwardingDefaultAndAllNICs(ipv6.ProtocolNumber, true); err != nil {
		t.Fatalf("replacement.SetForwardingDefaultAndAllNICs(ipv6): %s", err)
	}

	ext0EP, ext0Peer := veth.NewPair(1500, veth.DefaultBacklogSize)
	defer ext0Peer.Close()
	ext0EP.SetLinkAddress(ext0MAC)
	ext0Peer.SetLinkAddress(ext0PeerMAC)
	if err := replacement.CreateNICWithOptions(1, ethernet.New(ext0EP), stack.NICOptions{Name: "ext0"}); err != nil {
		t.Fatalf("replacement.CreateNICWithOptions(1): %s", err)
	}
	if err := replacement.AddProtocolAddress(1, tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: ext0V4,
	}, stack.AddressProperties{}); err != nil {
		t.Fatalf("replacement.AddProtocolAddress(1, ipv4): %s", err)
	}
	if err := replacement.AddProtocolAddress(1, tcpip.ProtocolAddress{
		Protocol:          ipv6.ProtocolNumber,
		AddressWithPrefix: ext0V6,
	}, stack.AddressProperties{}); err != nil {
		t.Fatalf("replacement.AddProtocolAddress(1, ipv6): %s", err)
	}
	if err := replacement.AddStaticNeighbor(1, ipv4.ProtocolNumber, ext0PeerV4, ext0PeerMAC); err != nil {
		t.Fatalf("replacement.AddStaticNeighbor(1, ipv4): %s", err)
	}
	if err := replacement.AddStaticNeighbor(1, ipv6.ProtocolNumber, ext0PeerV6, ext0PeerMAC); err != nil {
		t.Fatalf("replacement.AddStaticNeighbor(1, ipv6): %s", err)
	}

	ext1EP, ext1Peer := veth.NewPair(1500, veth.DefaultBacklogSize)
	defer ext1Peer.Close()
	ext1EP.SetLinkAddress(ext1MAC)
	if err := replacement.CreateNICWithOptions(4, ethernet.New(ext1EP), stack.NICOptions{Name: "ext1"}); err != nil {
		t.Fatalf("replacement.CreateNICWithOptions(4): %s", err)
	}

	ext0SubnetV4 := ext0V4.Subnet()
	ext0SubnetV6 := ext0V6.Subnet()
	ext1SubnetV4 := tcpip.AddressWithPrefix{Address: tcpip.AddrFrom4([4]byte{192, 168, 4, 0}), PrefixLen: 24}.Subnet()
	replacement.SetRouteTable([]tcpip.Route{
		{Destination: ext0SubnetV4, NIC: 1},
		{Destination: ext0SubnetV6, NIC: 1},
		{Destination: ext1SubnetV4, NIC: 4},
	})

	// Match Kernel.LoadFrom: ResetConfig before ReplaceConfig and Restore. Also
	// destroy replacement to ensure transferred NICs are detached from it.
	restored.ResetConfig()
	restored.ReplaceConfig(replacement)
	replacement.Destroy()
	restored.Restore()

	// Advance restored's clock so IPv6 DAD finishes on remapped ext0 (NIC 5) and
	// verify DAD reports NICID 5 on restored's NDPDispatcher rather than stale
	// NICID 1 on replacement.
	restoredClock := restored.Clock().(*faketime.ManualClock)
	restoredClock.Advance(time.Second)
	foundExt0DAD := false
	for len(dadDisp.dadCh) > 0 {
		ev := <-dadDisp.dadCh
		if ev.addr == ext0V6.Address {
			foundExt0DAD = true
			if ev.nicID != 5 {
				t.Errorf("DAD event for %s reported NICID %d, want 5", ext0V6.Address, ev.nicID)
			}
			if _, ok := ev.res.(*stack.DADSucceeded); !ok {
				t.Errorf("DAD event for %s result = %T, want *stack.DADSucceeded", ext0V6.Address, ev.res)
			}
		}
	}
	if !foundExt0DAD {
		t.Errorf("did not observe IPv6 DAD completion for %s on remapped NIC 5", ext0V6.Address)
	}

	// Preserved NICs 1 ("eth0") and 2 ("eth1") stay at 1 and 2; configured NIC 4
	// ("ext1") stays at 4; colliding configured NIC 1 ("ext0") moves past the
	// gap to ID 5.
	for _, want := range []struct {
		id   tcpip.NICID
		name string
	}{
		{id: 1, name: "eth0"},
		{id: 2, name: "eth1"},
		{id: 4, name: "ext1"},
		{id: 5, name: "ext0"},
	} {
		info, ok := restored.SingleNICInfo(want.id)
		if !ok || info.Name != want.name {
			t.Errorf("SingleNICInfo(%d) = (%+v, %v), want %q", want.id, info, ok, want.name)
		}
	}

	gotRoutes := make(map[tcpip.Subnet]tcpip.NICID)
	for _, r := range restored.GetRouteTable() {
		gotRoutes[r.Destination] = r.NIC
	}
	for subnet, wantNIC := range map[tcpip.Subnet]tcpip.NICID{
		eth0SubnetV4: 1,
		eth0SubnetV6: 1,
		ext1SubnetV4: 4,
		ext0SubnetV4: 5,
		ext0SubnetV6: 5,
	} {
		if got := gotRoutes[subnet]; got != wantNIC {
			t.Errorf("route for %s has NIC %d, want %d (routes=%+v)", subnet, got, wantNIC, restored.GetRouteTable())
		}
	}
	if next := restored.NextNICID(); next != 6 {
		t.Fatalf("NextNICID() after restore = %d, want 6", next)
	}

	// Verify IPv4 and IPv6 packet forwarding in both directions between
	// preserved NIC 1 ("eth0") and remapped NIC 5 ("ext0"), exercising
	// protocol.eps[5] lookup and rebound e.protocol on ext0.
	ext0Sink := &dstCaptureDispatcher{ch: make(chan tcpip.Address, 16)}
	ext0PeerEth := ethernet.New(ext0Peer)
	ext0PeerEth.Attach(ext0Sink)

	restoredCh1 := restored.GetLinkEndpointByName("eth0").(*ethernet.Endpoint).Child().(*channel.Endpoint)
	drainChannels(restoredCh1)

	v4ToExt0 := makeIPv4EthernetFrame(eth0PeerMAC, eth0MAC, eth0PeerV4, ext0PeerV4)
	restoredCh1.InjectInbound(header.IPv4ProtocolNumber, v4ToExt0)
	v4ToExt0.DecRef()

	v6ToExt0 := makeIPv6EthernetFrame(eth0PeerMAC, eth0MAC, eth0PeerV6, ext0PeerV6)
	restoredCh1.InjectInbound(header.IPv6ProtocolNumber, v6ToExt0)
	v6ToExt0.DecRef()

	for _, wantDst := range []tcpip.Address{ext0PeerV4, ext0PeerV6} {
		deadline := time.After(5 * time.Second)
		matched := false
		for !matched {
			select {
			case gotDst := <-ext0Sink.ch:
				if gotDst == wantDst {
					matched = true
				}
			case <-deadline:
				t.Fatalf("timed out waiting for packet forwarded to %s via remapped NIC 5", wantDst)
			}
		}
	}

	var inboundToExt0 stack.PacketBufferList
	inboundToExt0.PushBack(makeIPv4EthernetFrame(ext0PeerMAC, ext0MAC, ext0PeerV4, eth0PeerV4))
	inboundToExt0.PushBack(makeIPv6EthernetFrame(ext0PeerMAC, ext0MAC, ext0PeerV6, eth0PeerV6))
	if _, err := ext0Peer.WritePackets(inboundToExt0); err != nil {
		inboundToExt0.DecRef()
		t.Fatalf("ext0Peer.WritePackets failed: %s", err)
	}
	inboundToExt0.DecRef()

	for _, wantProto := range []tcpip.NetworkProtocolNumber{header.IPv4ProtocolNumber, header.IPv6ProtocolNumber} {
		var fwdPkt *stack.PacketBuffer
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if p := restoredCh1.Read(); p != nil {
				eth := header.Ethernet(p.LinkHeader().Slice())
				if eth.Type() == wantProto && eth.DestinationAddress() == eth0PeerMAC {
					fwdPkt = p
					break
				}
				p.DecRef()
			}
			time.Sleep(5 * time.Millisecond)
		}
		if fwdPkt == nil {
			t.Fatalf("timed out waiting for proto %d packet forwarded from remapped NIC 5 to preserved NIC 1", wantProto)
		}
		fwdPkt.DecRef()
	}

	// Closing ext1's and ext0's endpoints must invoke the rebound onCloseAction
	// on restored for both non-colliding NIC 4 and remapped colliding NIC 5.
	ext1EP.Close()
	if restored.HasNIC(4) {
		t.Errorf("HasNIC(4) = true after closing ext1 endpoint, want false")
	}
	ext0EP.Close()
	if restored.HasNIC(5) {
		t.Errorf("HasNIC(5) = true after closing ext0 endpoint, want false")
	}
}

func TestPreserveLoopbackDropsMovedUnpreservedNICAndResetsNFTablesConfigured(t *testing.T) {
	rootStack := stack.New(stack.Options{})
	defer rootStack.Destroy()
	childStack := stack.New(stack.Options{})

	loCh := channel.New(1, header.EthernetMinimumSize, "")
	loCh.LinkEPCapabilities = stack.CapabilityLoopback
	loID := childStack.NextNICID()
	if err := childStack.CreateNICWithOptions(loID, loCh, stack.NICOptions{Name: "lo"}); err != nil {
		t.Fatalf("childStack.CreateNICWithOptions(%d): %s", loID, err)
	}
	childStack.PreserveLoopback()
	childStack.SetNFTablesConfigured(true)

	// Simulate moving an unpreserved host-FD-backed NIC (empty Kind) from the
	// root stack into a child network namespace via RTM_SETLINK(IFLA_NET_NS_FD).
	ethCh := channel.New(1, header.EthernetMinimumSize, "\x02\x02\x03\x04\x05\x0c")
	if err := rootStack.CreateNICWithOptions(rootStack.NextNICID(), ethernet.New(ethCh), stack.NICOptions{Name: "eth0"}); err != nil {
		t.Fatalf("rootStack.CreateNICWithOptions: %s", err)
	}
	movedID, err := rootStack.SetNICStack(1, childStack)
	if err != nil {
		t.Fatalf("rootStack.SetNICStack(1, childStack): %s", err)
	}

	childStack.SetRemoveConf(true)
	childStack.Pause()
	var buf bytes.Buffer
	if _, err := state.Save(t.Context(), &buf, childStack); err != nil {
		t.Fatalf("state.Save failed: %v", err)
	}
	childStack.Destroy()

	var restoredChild stack.Stack
	if _, err := state.Load(t.Context(), bytes.NewReader(buf.Bytes()), &restoredChild); err != nil {
		t.Fatalf("state.Load failed: %v", err)
	}
	restoredChild.Restore()
	defer restoredChild.Destroy()

	if !restoredChild.HasNIC(loID) {
		t.Fatalf("restoredChild.HasNIC(%d) = false, want true for preserved loopback NIC", loID)
	}
	if restoredChild.HasNIC(movedID) {
		t.Fatalf("restoredChild.HasNIC(%d) = true, want false for unpreserved NIC moved into child stack", movedID)
	}
	if restoredChild.IsNFTablesConfigured() {
		t.Fatal("restoredChild.IsNFTablesConfigured() = true, want false after load")
	}
}

func TestMain(m *testing.M) {
	refs.SetLeakMode(refs.LeaksPanic)
	code := m.Run()
	refs.DoLeakCheck()
	os.Exit(code)
}
