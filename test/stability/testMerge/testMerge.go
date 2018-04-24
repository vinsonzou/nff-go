// Copyright 2017 Intel Corporation.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"errors"
	"flag"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/intel-go/nff-go/flow"
	"github.com/intel-go/nff-go/packet"
	"github.com/intel-go/nff-go/test/stability/stabilityCommon"
)

// Test with testScenario=1:
// This part of test generates packets on ports 0 and 1, receives packets
// on 0 port. Packets generated on 0 port has IPv4 source addr ipv4addr1,
// and those generated on 1 port has ipv4 source addr ipv4addr2. For each packet
// sender calculates IPv4 and UDP checksums and verify it on packet receive.
// When packet is received, hash is recomputed and checked if it is equal to value
// in the packet. Test also calculates sent/received ratios, number of broken
// packets and prints it when a predefined number of packets is received.
//
// Test with testScenario=2:
// This part of test receives packets on 0 and 1 ports, merges flows
// and send result flow to 0 port.
//
// Test with testScenario=0:
// all these actions are made in one pipeline without actual send and receive.

const (
	gotest uint = iota
	generatePart
	receivePart
	useGenerator1 = 0
	useGenerator2 = 1
)

var (
	totalPackets uint64 = 10000000
	// Payload is 16 byte md5 hash sum of headers
	payloadSize uint   = 16
	speed       uint64 = 1000000
	passedLimit uint64 = 85

	sentPacketsGroup1 uint64
	sentPacketsGroup2 uint64
	sentCopiedGroup1 uint64
	sentCopiedGroup2 uint64

	recvPacketsGroup1 uint64
	recvPacketsGroup2 uint64
	recvCopyPacketsGroup1 uint64
	recvCopyPacketsGroup2 uint64

	recvPackets       uint64
	brokenPackets     uint64

	// Usually when writing multibyte fields to packet, we should make
	// sure that byte order in packet buffer is correct and swap bytes if needed.
	// Here for testing purposes we use addresses with bytes swapped by hand.
	ipv4addr1 uint32 = 0x0100007f // 127.0.0.1
	ipv4addr2 uint32 = 0x05090980 // 128.9.9.5

	testDoneEvent *sync.Cond
	progStart     time.Time

	// T second timeout is used to let generator reach required speed
	// During timeout packets are skipped and not counted
	T = 10 * time.Second

	outport1     uint
	outport2     uint
	inport1      uint
	inport2      uint
	dpdkLogLevel = "--log-level=0"
	outFile		string

	fixMACAddrs  func(*packet.Packet, flow.UserContext)
	fixMACAddrs1 func(*packet.Packet, flow.UserContext)
	fixMACAddrs2 func(*packet.Packet, flow.UserContext)
	passed       int32 = 1
	testScenario uint
	wasFlow1Stopped = false
	wasFlow2Stopped = false
)

type payloadVal uint
const (
	notCount payloadVal = iota
	baseGr1
	baseGr2
	copiedGr1
	copiedGr2
)

type packetData struct {
	N uint
}

func main() {
	var pipelineType, generatorType uint
	var addSegmAfterMerge bool
	flag.UintVar(&generatorType, "generatorType", 1, "generator configurations: 0 to use simple generator, 1 to use fast generator")
	flag.BoolVar(&addSegmAfterMerge, "addSegmAfterMerge", false, "1 to insert handler after merge in pipeline, 0 otherwise")
	flag.UintVar(&testScenario, "testScenario", 0, "1 to use 1st part scenario, 2 snd, 0 to use one-machine test")
	flag.UintVar(&pipelineType, "pipelineType", 0, "merge between different ff types: 0 - getGet, 1 - inside Copy, 2- between Copy, 3 - between Segments, 4 - segm and get, 5 - segm and copy, 6 - inside Segm, 7 - between manyFlows")
	flag.Uint64Var(&passedLimit, "passedLimit", passedLimit, "received/sent minimum ratio to pass test")
	flag.Uint64Var(&speed, "speed", speed, "speed of 1 and 2 generators, Pkts/s")
	flag.UintVar(&outport1, "outport1", 0, "port for 1st sender")
	flag.UintVar(&outport2, "outport2", 1, "port for 2nd sender")
	flag.UintVar(&inport1, "inport1", 0, "port for 1st receiver")
	flag.UintVar(&inport2, "inport2", 1, "port for 2nd receiver")
	flag.Uint64Var(&totalPackets, "number", totalPackets, "total number of packets to receive by test")
	flag.DurationVar(&T, "timeout", T, "test start delay, needed to stabilize speed. Packets sent during timeout do not affect test result")
	configFile := flag.String("config", "", "Specify json config file name (mandatory for VM)")
	target := flag.String("target", "", "Target host name from config file (mandatory for VM)")
	dpdkLogLevel = *(flag.String("dpdk", "--log-level=0", "Passes an arbitrary argument to dpdk EAL"))
	flag.Parse()
	flow.CheckFatal(initDPDK())
	flow.CheckFatal(executeTest(*configFile, *target, testScenario, pipelineScenario(pipelineType), getScenario(generatorType), addSegmAfterMerge))
}


// getter - is a part of pipeline getting packets by receive or generate or fast generate
type getScenario uint
const (
	generate getScenario = iota
	fastGenerate
	recv
)

func setGetter(scenario getScenario, inputSource uint) (finalFlow *flow.Flow, err error){
	switch scenario {
	case recv:
		finalFlow, err = flow.SetReceiver(uint16(inputSource))
	case fastGenerate:
		if inputSource == useGenerator1 {
			finalFlow, err = flow.SetFastGenerator(generatePacketGroup1, speed, nil)
		} else if inputSource == useGenerator2 {
			finalFlow, err = flow.SetFastGenerator(generatePacketGroup2, speed, nil)
		} else {
			return nil, errors.New(" setGetter: unknown generator type")
		}
	default:
		if inputSource == useGenerator1 {
			finalFlow = flow.SetGenerator(generatePacketGroup1, nil)
		} else if inputSource == useGenerator2 {
			finalFlow = flow.SetGenerator(generatePacketGroup2, nil)
		} else {
			return nil, errors.New(" setGetter: unknown generator type")
		}
	}
	return finalFlow, err
}

// segment type definition, not all possible values are used to decrease complexity
type segmentScenario uint
const (
	handle segmentScenario = iota
	handleDrop
	split
)

// returns nil if no new flow was created and new flow if was
func setSegment(scenario segmentScenario, f *flow.Flow) (finalFlow *flow.Flow, err error){
	err = errors.New("Unrecognized configuration of segment part")
	switch scenario {
	case handle:
		err = flow.SetHandler(f, handlePackets, nil)
	case handleDrop:
		err = flow.SetHandlerDrop(f, filterPackets, nil)
	case split:
		var outputFlows []*flow.Flow
		outputFlows, err = flow.SetSplitter(f, splitPackets, uint(2), nil)
		if err != nil {
			return nil, err
		}
		finalFlow = outputFlows[1]
		*f = *outputFlows[0]
	}
	return finalFlow, err
}

func splitPackets(currentPacket *packet.Packet, context flow.UserContext) uint {
	return 0
}

func handlePackets(pkt *packet.Packet, context flow.UserContext) {
}

func filterPackets(pkt *packet.Packet, context flow.UserContext) bool {
	return true
}

// different test cases
type pipelineScenario uint
const (
	getGet pipelineScenario = iota
	inCopy
	copyCopy
	segmSegm
	segmGet
	segmCopy
	inSegm
	manyFlows
)

// set copier and handler to count packets in copied flow for future statistics
func setCopyWithCounter(inFlow *flow.Flow) (*flow.Flow, error) {
	outFlow, err := flow.SetCopier(inFlow)
	if err != nil {
		return nil, err
	}
	err = flow.SetHandler(outFlow, countCopiedPackets, nil)
	return outFlow, err
}

// stop flow and do not count packets in statistics
func stopFlow(inFlow *flow.Flow, isGroup1 bool) error {
	if isGroup1 {
		wasFlow1Stopped = true
	} else {
		wasFlow2Stopped = true
	}
	return flow.SetStopper(inFlow)
}

func buildPipeline(scenario pipelineScenario, firstFlow, secondFlow *flow.Flow) (*flow.Flow, error){
	var err error
	switch scenario {
	// simplest case - two getters are merged
	case getGet:
	// flow merged with its copy
	case inCopy:
		err = stopFlow(secondFlow, false)
		if err != nil {
			return nil, err
		}
		secondFlow, err = setCopyWithCounter(firstFlow)
	// two flows are copied and all merged
	case copyCopy:
		firstFlowCopy, err := setCopyWithCounter(firstFlow)
		if err != nil {
			return nil, err
		}
		secondFlowCopy, err := setCopyWithCounter(secondFlow)
		if err != nil {
			return nil, err
		}
		return flow.SetMerger(firstFlow, secondFlow, firstFlowCopy, secondFlowCopy)
	// flow is splitted and then merged
	case inSegm:
		err = stopFlow(secondFlow, false)
		if err != nil {
			return nil, err
		}
		secondFlow, err = setSegment(split, firstFlow)
	// segment merged with not segment
	case segmGet:
		_, err = setSegment(handle, firstFlow)
	// segmend merged with copy
	case segmCopy:
		err = stopFlow(secondFlow, false)
		if err != nil {
			return nil, err
		}
		_, err = setSegment(handleDrop, firstFlow)
		if err != nil {
			return nil, err
		}
		secondFlow, err = setCopyWithCounter(firstFlow)
	// two segments merged
	case segmSegm:
		_, err = setSegment(handle, firstFlow)
		if err != nil {
			return nil, err
		}
		_, err = setSegment(handleDrop, secondFlow)
	// many flows are merged
	case manyFlows:
		splittedFlow, err := setSegment(split, firstFlow)
		if err != nil {
			return nil, err
		}
		copiedFlow, err := setCopyWithCounter(secondFlow)
		if err != nil {
			return nil, err
		}
		err = stopFlow(secondFlow, false)
		if err != nil {
			return nil, err
		}
		return flow.SetMerger(firstFlow, splittedFlow, copiedFlow)
	}
	return flow.SetMerger(firstFlow, secondFlow)
}

// choice how to end the flow
type finishScenario uint
const (
	send finishScenario = iota
	stop
)

func finishPipeline(scenario finishScenario, f *flow.Flow, port uint) error {
	switch scenario {
	case send:
		return flow.SetSender(f, uint16(port))
	}
	return flow.SetStopper(f)
}

// test body
func setTestingPart(scenario pipelineScenario, getter getScenario, addSegmAfterMerge bool) error {
	var input1, input2 uint
	var m sync.Mutex
	var checkedFlow *flow.Flow
	var err error
	if testScenario != receivePart {
		testDoneEvent = sync.NewCond(&m)
		input1 = useGenerator1
		input2 = useGenerator2
		if getter == recv {
			return errors.New("wrong setTestingPart configuration, getter should not be receive")
		}
	} else {
		input1 = inport1
		input2 = inport2
		getter = recv
	}
	firstFlow, err := setGetter(getter, input1)
	if err != nil {
		return err
	}
	secondFlow, err := setGetter(getter, input2)
	if err != nil {
		return err
	}
	if testScenario != generatePart {
		checkedFlow, err = buildPipeline(scenario, firstFlow, secondFlow)
		if err != nil {
			return err
		}
		// add segment to pipeline after merge function
		if addSegmAfterMerge {
			if _, err := setSegment(handle, checkedFlow); err != nil {
				return err
			}
		}
	} else {
		if err := finishPipeline(send, firstFlow, outport1); err != nil {
			return err
		}
		if err := finishPipeline(send, secondFlow, outport2); err != nil {
			return err
		}
		checkedFlow, err = setGetter(recv, inport1)
		if err != nil {
			return err
		}
	}
	if testScenario != receivePart{
		if err := flow.SetHandler(checkedFlow, checkPackets, nil); err != nil {
			return err
		}
	} else {
		if err := flow.SetHandler(checkedFlow, fixPackets, nil); err != nil {
			return err
		}
	}
	if err := finishPipeline(stop, checkedFlow, outport1); err != nil {
		return err
	}
	if testScenario == receivePart {
		return flow.SystemStart()
	}
	// Start pipeline
	go func() {
		err = flow.SystemStart()
	}()
	if err != nil {
		return err
	}
	progStart = time.Now()

	// Wait for enough packets to arrive
	testDoneEvent.L.Lock()
	testDoneEvent.Wait()
	testDoneEvent.L.Unlock()

	return composeStatistics(scenario)
}

func resetState() {
	flow.SystemStop()

	sentPacketsGroup1 = 0
	sentPacketsGroup2 = 0
	sentCopiedGroup1 = 0
	sentCopiedGroup2 = 0

	recvPacketsGroup1 = 0
	recvPacketsGroup2 = 0
	recvCopyPacketsGroup1 = 0
	recvCopyPacketsGroup2 = 0

	recvPackets       = 0
	brokenPackets     = 0

	passed      	  = 1
	
	wasFlow1Stopped = false
	wasFlow2Stopped = false
}

func initDPDK() error {
	// Init NFF-GO system
	config := flow.Config{
		DPDKArgs: []string{dpdkLogLevel},
	}
	return flow.SystemInit(&config)
}

func executeTest(configFile, target string, testScenario uint, scenario pipelineScenario, generatorType getScenario, addSegmAfterMerge bool) error {
	if testScenario > 3 || testScenario < 0 {
		return errors.New("testScenario should be in interval [0, 3]")
	}
	stabilityCommon.InitCommonState(configFile, target)

	fixMACAddrs1 = stabilityCommon.ModifyPacket[outport1].(func(*packet.Packet, flow.UserContext))
	fixMACAddrs2 = stabilityCommon.ModifyPacket[outport2].(func(*packet.Packet, flow.UserContext))
	fixMACAddrs = stabilityCommon.ModifyPacket[outport1].(func(*packet.Packet, flow.UserContext))
	return setTestingPart(scenario, generatorType, addSegmAfterMerge)
}

func failCheck(msg string) error {
	println("TEST FAILED")
	return errors.New(msg)
}

func composeStatistics(scenario pipelineScenario) error {
	maxBroken := totalPackets / 5
	lessPercent := 50
	eps := 4
	var sent1, sent2 uint64
	// Compose statistics
	if !wasFlow1Stopped{
		sent1 = atomic.LoadUint64(&sentPacketsGroup1)
	}
	if !wasFlow2Stopped{
		sent2 = atomic.LoadUint64(&sentPacketsGroup2)
	}

	copied1 := atomic.LoadUint64(&sentCopiedGroup1)
	copied2 := atomic.LoadUint64(&sentCopiedGroup2)

	if scenario == copyCopy || scenario == segmCopy {
		// generate 2 flows, copy both, speed of generation is much more than speed of accepting and checking
		passedLimit = 50
	}
	sent := sent1 + sent2
	copied := copied1 + copied2
	if (sent + copied) == 0 {
		return failCheck("Sent 0 packets, error!")
	}
	
	recv1 := atomic.LoadUint64(&recvPacketsGroup1)
	recv2 := atomic.LoadUint64(&recvPacketsGroup2)
	recvCopy1 := atomic.LoadUint64(&recvCopyPacketsGroup1)
	recvCopy2 := atomic.LoadUint64(&recvCopyPacketsGroup2)
	received := recv1 + recv2 + recvCopy1 + recvCopy2
	if received == 0 {
		return failCheck("received 0 packets, error!")
	}
	// Proportions of 1 and 2 packet in received flow
	p1 := int((recv1+recvCopy1) * 100 / received)
	p2 := int((recv2+recvCopy2) * 100 / received)
	broken := atomic.LoadUint64(&brokenPackets)

	// Print report
	println("=================================")
	println("Sent total:", sent + copied, "packets")
	println("generated group 1", sent1, "packets")
	println("copied group 1", copied1, "packets")
	println("generated group 2", sent2, "packets")
	println("copied group 2", copied2, "packets")
	println("=================================")
	println("Received total:", received, "packets")
	println("got group 1", recv1, "packets")
	println("got copied group 1", recvCopy1, "packets")
	println("got group 2", recv2, "packets")
	println("got copied group 2", recvCopy2, "packets")
	println("=================================")

	var ratio1, ratio2 uint64
	if sent1 + copied1 != 0 {
		ratio1 = (recv1+recvCopy1)*100/(sent1 + copied1)
	} else {
		if (recv1+recvCopy1) != 0 {
			return failCheck("lost all packets from group 1")
		}
	}

	if sent2 + copied2 != 0 {
		ratio2 = (recv2+recvCopy2)*100/(sent2 + copied2)
	} else {
		if (recv2+recvCopy2) != 0 {
			return failCheck("lost all packets from group 2")
		}
	}
	println("Group1 ratio =", ratio1, "%")
	println("Group2 ratio =", ratio2, "%")

	println("Group1 proportion in received flow =", p1, "%")
	println("Group2 proportion in received flow =", p2, "%")

	println("Broken = ", broken, "packets")
	
	if broken > maxBroken {
		return failCheck("too many broken packets")
	}

	if sent1 == 0 && recv1 != 0 || sent2 == 0 && recv2 != 0 || copied1 == 0 && recvCopy1 != 0 || copied2 == 0 && recvCopy2 != 0 {
		return failCheck("something went wrong..sent 0, but received not 0 in some of groups")
	}

	ok := false
	if atomic.LoadInt32(&passed) != 0 && received*100/(sent+copied) > passedLimit {
		if scenario == inCopy || scenario == segmCopy || scenario == inSegm && p2 == 0 && p1 == 100 {
			ok = true
		} else {
			// Test is passed, if p1 and p2 do not differ too much: |p1-p2| < 4%
			// and enough packets received back
			if p1 <= lessPercent+eps && p2 <= 100-lessPercent+eps &&
			p1 >= lessPercent-eps && p2 >= 100-lessPercent-eps {
				ok = true
			}
		}
	}
	if ok {
		println("TEST PASSED")
		return nil
	}
	return failCheck("final statistics check failed")
}

func fixPackets(pkt *packet.Packet, ctx flow.UserContext) {
	if stabilityCommon.ShouldBeSkipped(pkt) {
		return
	}
	fixMACAddrs(pkt, ctx)
}


func generatePacketGroup1(pkt *packet.Packet, context flow.UserContext) {
	if pkt == nil {
		log.Fatal("Failed to create new packet")
	}
	if packet.InitEmptyIPv4UDPPacket(pkt, payloadSize) == false {
		log.Fatal("Failed to init empty packet")
	}
	ipv4 := pkt.GetIPv4()
	udp := pkt.GetUDPForIPv4()
	ipv4.SrcAddr = ipv4addr1

	fixMACAddrs1(pkt, context)

	ptr := (*packetData)(pkt.Data)
	ptr.N = uint(notCount)

	// We do not consider the start time of the system in this test
	if time.Since(progStart) >= T && atomic.LoadUint64(&recvPackets) < totalPackets {
		ptr.N = uint(baseGr1)
		atomic.AddUint64(&sentPacketsGroup1, 1)
	}

	ipv4.HdrChecksum = packet.SwapBytesUint16(packet.CalculateIPv4Checksum(ipv4))
	udp.DgramCksum = packet.SwapBytesUint16(packet.CalculateIPv4UDPChecksum(ipv4, udp, pkt.Data))
}

func generatePacketGroup2(pkt *packet.Packet, context flow.UserContext) {
	if pkt == nil {
		log.Fatal("Failed to create new packet")
	}
	if packet.InitEmptyIPv4UDPPacket(pkt, payloadSize) == false {
		log.Fatal("Failed to init empty packet")
	}
	ipv4 := pkt.GetIPv4()
	udp := pkt.GetUDPForIPv4()
	ipv4.SrcAddr = ipv4addr2

	fixMACAddrs2(pkt, context)

	ptr := (*packetData)(pkt.Data)
	ptr.N = uint(notCount)
	// We do not consider the start time of the system in this test
	if time.Since(progStart) >= T && atomic.LoadUint64(&recvPackets) < totalPackets {
		ptr.N = uint(baseGr2)
		atomic.AddUint64(&sentPacketsGroup2, 1)
	}
	ipv4.HdrChecksum = packet.SwapBytesUint16(packet.CalculateIPv4Checksum(ipv4))
	udp.DgramCksum = packet.SwapBytesUint16(packet.CalculateIPv4UDPChecksum(ipv4, udp, pkt.Data))
}

// Count and check packets in received flow
func checkPackets(pkt *packet.Packet, context flow.UserContext) {
	if time.Since(progStart) < T || stabilityCommon.ShouldBeSkipped(pkt) {
		return
	}

	if pkt.ParseData() < 0 {
		println("ParseData returned negative value")
		atomic.AddUint64(&brokenPackets, 1)
		return
	}

	ipv4 := pkt.GetIPv4()
	udp := pkt.GetUDPForIPv4()
	recvIPv4Cksum := packet.SwapBytesUint16(packet.CalculateIPv4Checksum(ipv4))
	recvUDPCksum := packet.SwapBytesUint16(packet.CalculateIPv4UDPChecksum(ipv4, udp, pkt.Data))
	ptr := (*packetData)(pkt.Data)
	payload := payloadVal(ptr.N)
	if payload == notCount {
		return
	}
	
	if recvIPv4Cksum != ipv4.HdrChecksum || recvUDPCksum != udp.DgramCksum {
		// Packet is broken
		atomic.AddUint64(&brokenPackets, 1)
		return
	}
	if ipv4.SrcAddr == ipv4addr1 {
		if payload == baseGr1 {
			atomic.AddUint64(&recvPacketsGroup1, 1)
		} else if payload == copiedGr1 {
			atomic.AddUint64(&recvCopyPacketsGroup1, 1)
		} else {
			atomic.AddUint64(&brokenPackets, 1)
		}
	} else if ipv4.SrcAddr == ipv4addr2 {
		if payload == baseGr2 {
			atomic.AddUint64(&recvPacketsGroup2, 1)
		} else if payload == copiedGr2 {
			atomic.AddUint64(&recvCopyPacketsGroup2, 1)
		} else {
			atomic.AddUint64(&brokenPackets, 1)
		}
	} else {
		println("Packet Ipv4 src addr does not match addr1 or addr2")
		println("TEST FAILED")
		atomic.StoreInt32(&passed, 0)
	}
	if atomic.AddUint64(&recvPackets, 1) > totalPackets {
		testDoneEvent.Signal()
		return
	}
}

// Count and check packets in received flow
func countCopiedPackets(pkt *packet.Packet, context flow.UserContext) {
	if time.Since(progStart) >= T && !stabilityCommon.ShouldBeSkipped(pkt) && atomic.LoadUint64(&recvPackets) < totalPackets {
		pkt.ParseData()
		ptr := (*packetData)(pkt.Data)
		if ptr.N != uint(notCount) {
			if ptr.N % 2 == 0 {
				ptr.N = uint(copiedGr2)
				atomic.AddUint64(&sentCopiedGroup2, 1)
			} else {
				ptr.N = uint(copiedGr1)
				atomic.AddUint64(&sentCopiedGroup1, 1)
			}
			ipv4 := pkt.GetIPv4()
			udp := pkt.GetUDPForIPv4()
			ipv4.HdrChecksum = packet.SwapBytesUint16(packet.CalculateIPv4Checksum(ipv4))
			udp.DgramCksum = packet.SwapBytesUint16(packet.CalculateIPv4UDPChecksum(ipv4, udp, pkt.Data))
		}
	}
}
