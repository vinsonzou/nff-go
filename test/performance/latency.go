// Copyright 2017 Intel Corporation.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"flag"
	"fmt"
	"log"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/intel-go/nff-go/common"
	"github.com/intel-go/nff-go/flow"
	"github.com/intel-go/nff-go/packet"
)

// This is 1 part of latency test
// For 2 part can be any of perf_light, perf_main, perf_seq
//
// latency-part1:
// This part of test generates packets, puts time of generation into packet
// and send.
// Received flow is partitioned in skipNumber:1 relation. For this 1 packet latency is
// calculated (current time minus time in packet) and then reported to channel.
// Test is finished when number of reported latency measurements == latNumber

// These are default test settings. Can be modified with command line.
var (
	latNumber          = 500
	skipNumber  uint64 = 100000
	speed       uint64 = 1000000
	passedLimit uint64 = 80

	packetSize   uint64 = 128
	servDataSize uint64 = common.EtherLen + common.IPv4MinLen + common.UDPLen + crcLen

	outport uint16
	inport  uint16

	dpdkLogLevel = "--log-level=0"
)

const (
	crcLen = 4

	dstPort1 uint16 = 111
	dstPort2 uint16 = 222
)

var (
	// packetSize - servDataSize
	payloadSize uint64
	// Counters of sent packets
	sentPackets uint64

	// Received flow is partitioned in two flows. Each flow has separate packet counter:
	// Counter of received packets, for which latency calculated
	checkedPktsCount uint64
	// Counter of other packets
	uncheckedPktsCount uint64

	// Event is to notify when latNumber number of packets are received
	testDoneEvent *sync.Cond
	// Channel is used to report packet latencies
	latencies chan time.Duration
	// Channel to stop collecting latencies and print statistics
	stop chan string
	// Latency values are stored here for next processing
	latenciesStorage []time.Duration
)

func main() {
	flag.IntVar(&latNumber, "latNumber", latNumber, "number of packets, for which latency should be reported")
	flag.Uint64Var(&skipNumber, "skipNumber", skipNumber, "test calculates latency only for 1 of skipNumber packets")
	flag.Uint64Var(&speed, "speed", speed, "speed of generator")
	flag.Uint64Var(&passedLimit, "passedLimit", passedLimit, "received/sent minimum ratio to pass test")
	flag.Uint64Var(&packetSize, "packetSize", packetSize, "size of packet")
	outport = uint16(*flag.Uint("outport", 0, "port for sender"))
	inport = uint16(*flag.Uint("inport", 1, "port for receiver"))
	dpdkLogLevel = *(flag.String("dpdk", "--log-level=0", "Passes an arbitrary argument to dpdk EAL"))
	flag.Parse()

	latencies = make(chan time.Duration)
	stop = make(chan string)
	latenciesStorage = make([]time.Duration, latNumber)

	go latenciesLogger(latencies, stop)

	// Event needed to stop sending packets
	var m sync.Mutex
	testDoneEvent = sync.NewCond(&m)

	// Initialize NFF-GO library
	if err := initDPDK(); err != nil {
		fmt.Printf("fail: %+v\n", err)
	}
	payloadSize = packetSize - servDataSize

	// Create packet flow
	outputFlow, err := flow.SetFastGenerator(generatePackets, speed, nil)
	flow.CheckFatal(err)
	outputFlow2, err := flow.SetPartitioner(outputFlow, 350, 350)
	flow.CheckFatal(err)

	flow.CheckFatal(flow.SetSender(outputFlow, outport))
	flow.CheckFatal(flow.SetSender(outputFlow2, outport))

	// Create receiving flow and set a checking function for it
	inputFlow, err := flow.SetReceiver(inport)
	flow.CheckFatal(err)

	// Calculate latency only for 1 of skipNumber packets
	latFlow, err := flow.SetPartitioner(inputFlow, skipNumber, 1)
	flow.CheckFatal(err)

	flow.CheckFatal(flow.SetHandler(latFlow, checkPackets, nil))
	flow.CheckFatal(flow.SetHandler(inputFlow, countPackets, nil))

	flow.CheckFatal(flow.SetStopper(inputFlow))
	flow.CheckFatal(flow.SetStopper(latFlow))

	// Start pipeline
	go func() {
		flow.CheckFatal(flow.SystemStart())
	}()

	// Wait until enough latencies calculated
	testDoneEvent.L.Lock()
	testDoneEvent.Wait()
	testDoneEvent.L.Unlock()

	// Compose statistics
	composeStatistics()
}

func initDPDK() error {
	// Init NFF-GO system
	config := flow.Config{
		DPDKArgs: []string{dpdkLogLevel},
	}
	if err := flow.SystemInit(&config); err != nil {
		return err
	}
	return nil
}

func composeStatistics() {
	sent := atomic.LoadUint64(&sentPackets)
	checked := atomic.LoadUint64(&checkedPktsCount)
	ignored := atomic.LoadUint64(&uncheckedPktsCount)
	received := checked + ignored

	ratio := received * 100 / sent

	fmt.Println("Packet size", packetSize, "bytes")
	fmt.Println("Requested speed", speed)
	fmt.Println("Sent", sent, "packets")
	fmt.Println("Received (total)", received, "packets")
	fmt.Println("Received/sent ratio =", ratio, "%")
	fmt.Println("Checked", checked, "packets")

	stat := calcStats(latenciesStorage)
	fmt.Println("Median = ", stat.median)
	fmt.Println("Average = ", stat.average)
	fmt.Println("Stddev = ", stat.stddev)

	if ratio > passedLimit {
		fmt.Println("TEST PASSED")
	} else {
		fmt.Println("TEST FAILED")
	}
}

func generatePackets(pkt *packet.Packet, context flow.UserContext) {
	if pkt == nil {
		fmt.Println("TEST FAILED")
		log.Fatal("Failed to create new packet")
	}
	if packet.InitEmptyIPv4UDPPacket(pkt, uint(payloadSize)) == false {
		fmt.Println("TEST FAILED")
		log.Fatal("Failed to init empty packet")
	}
	ipv4 := pkt.GetIPv4()
	udp := pkt.GetUDPForIPv4()

	// We need different packets to gain from RSS
	if atomic.LoadUint64(&sentPackets)%2 == 0 {
		udp.DstPort = packet.SwapBytesUint16(dstPort1)
	} else {
		udp.DstPort = packet.SwapBytesUint16(dstPort2)
	}

	ptr := (*packetData)(pkt.Data)
	ptr.SendTime = time.Now()

	ipv4.HdrChecksum = packet.SwapBytesUint16(packet.CalculateIPv4Checksum(ipv4))
	udp.DgramCksum = packet.SwapBytesUint16(packet.CalculateIPv4UDPChecksum(ipv4, udp, pkt.Data))

	if atomic.LoadUint64(&checkedPktsCount) < uint64(latNumber) {
		atomic.AddUint64(&sentPackets, 1)
	}
}

// This function take latencies from channel, and put values to array.
func latenciesLogger(ch <-chan time.Duration, stop <-chan string) {
	sum := time.Duration(0)
	count := 0
	for {
		select {
		case lat := <-ch:
			sum += lat
			latenciesStorage[count] = lat
			count++
		case _ = <-stop:
			testDoneEvent.Signal()
			break
		}
	}
}

// Check packets in received flow, calculate latencies and put to chennel
func checkPackets(pkt *packet.Packet, context flow.UserContext) {
	checkCount := atomic.LoadUint64(&checkedPktsCount)
	if pkt.ParseData() < 0 {
		fmt.Println("Cannot parse Data, skip")
	} else if checkCount >= uint64(latNumber) {
		stop <- "stop"
	} else {
		ptr := (*packetData)(pkt.Data)
		atomic.AddUint64(&checkedPktsCount, 1)
		RecvTime := time.Now()
		lat := RecvTime.Sub(ptr.SendTime)
		// Report latency
		latencies <- lat
	}
}

func countPackets(pkt *packet.Packet, context flow.UserContext) {
	atomic.AddUint64(&uncheckedPktsCount, 1)
}

type packetData struct {
	PktLabel uint32
	SendTime time.Time
}

type latencyStat struct {
	median  time.Duration
	average time.Duration
	stddev  time.Duration
}

// Calculate median, average and stddev
func calcStats(times []time.Duration) (stat latencyStat) {
	stat.median = median(times)

	g := time.Duration(0)
	for i := 0; i < latNumber; i++ {
		g += times[i]
	}
	stat.average = time.Duration(int64(g) / int64(latNumber))

	s := float64(0)
	for i := 0; i < latNumber; i++ {
		s += math.Pow(float64(times[i])-float64(stat.average), 2)
	}
	stat.stddev = time.Duration(math.Sqrt(s / float64(latNumber)))
	return
}

func median(arr []time.Duration) time.Duration {
	len := len(arr)
	tmp := make([]time.Duration, len)
	copy(tmp, arr)
	sort.Slice(tmp, func(i, j int) bool { return tmp[i] < tmp[j] })
	if len%2 == 0 {
		return tmp[len/2]/2 + tmp[len/2+1]/2
	} else {
		return tmp[len/2+1]
	}
}
