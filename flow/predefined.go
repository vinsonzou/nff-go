// Copyright 2017 Intel Corporation.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package flow is the main package of NFF-GO library and should be always imported by
// user application.
package flow

import (
	"github.com/intel-go/nff-go/common"
	"github.com/intel-go/nff-go/packet"
)


// TODO we should put exact port here. However now we don't have parameters.
// HandleARP function should be used inside SetHandleDrop flow function
// for dealing with ARP requests
func HandleARP(current *packet.Packet, context *UserContext) bool {
	current.ParseL3()
	arp := current.GetARPCheckVLAN()
	// ARP can be only in IPv4. IPv6 replace it with modified ICMP
	if arp != nil {
		if packet.SwapBytesUint16(arp.Operation) != packet.ARPRequest {
			// TODO Implement our own requests and answers for them later.
			// We don't care about replies so far
			return false
		}

		if arp.THA != [common.EtherAddrLen]byte{} {
			println("Warning! Got an ARP packet with non-zero MAC address", packet.MACToString(arp.THA),
				". ARP request ignored.")
			return false
		}

		l, _ := portsTable.Load(packet.ArrayToIPv4(arp.TPA))
		if l == nil {
			//println("Warning! Got an ARP packet with target IPv4 address", StringIPv4Array(arp.TPA),
			//        "different from IPv4 address on interface. Should be", StringIPv4Int(port.Subnet.Addr),
			//        ". ARP request ignored.")
			return false
		}
		p := l.(*port)

		// Prepare an answer to this request
		answerPacket, err := packet.NewPacket()
		if err != nil {
			common.LogFatal(common.Debug, err)
		}
		packet.InitARPReplyPacket(answerPacket, p.MAC, arp.SHA, packet.ArrayToIPv4(arp.TPA), packet.ArrayToIPv4(arp.SPA))
		answerPacket.SendPacket(p.port)

		return false
	}
	return true
}
