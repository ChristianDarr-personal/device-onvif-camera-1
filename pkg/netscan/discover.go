// -*- Mode: Go; indent-tabs-mode: t -*-
//
// Copyright (C) 2022 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0

package netscan

import (
	"context"
	"encoding/binary"
	"github.com/pkg/errors"
	"math"
	"net"
	"sync"
	"syscall"
	"time"
)

const (
	NetworkUDP = "udp"
	NetworkTCP = "tcp"
)

// AutoDiscover probes all addresses in the configured network to attempt to discover any possible
// devices for a specific protocol
func AutoDiscover(ctx context.Context, proto ProtocolSpecificDiscovery, params Params) []DiscoveredDevice {
	params.Logger.Debugf("AutoDiscover called with the following parameters: %+v", params)
	if len(params.Subnets) == 0 {
		params.Logger.Warnf("Discover was called, but no subnet information has been configured!")
		return nil
	}

	ipnets := make([]*net.IPNet, 0, len(params.Subnets))
	var estimatedProbes int
	for _, cidr := range params.Subnets {
		if cidr == "" {
			params.Logger.Warnf("Empty CIDR provided, unable to scan for Onvif cameras.")
			continue
		}

		ip, ipnet, err := net.ParseCIDR(cidr)
		if err != nil {
			params.Logger.Errorf("Unable to parse CIDR %q: %s", cidr, err)
			continue
		}
		if ip == nil || ipnet == nil || ip.To4() == nil {
			params.Logger.Errorf("Currently only ipv4 subnets are supported. subnet=%q", cidr)
			continue
		}

		ipnets = append(ipnets, ipnet)
		// compute the estimate total amount of network probes we are going to make
		// this is an estimate because it may be lower due to skipped addresses (existing devices)
		sz, _ := ipnet.Mask.Size()
		estimatedProbes += int(computeNetSz(sz))
	}

	// if the estimated amount of probes we are going to make is less than
	// the async limit, we only need to set the worker count to the total number
	// of probes to avoid spawning more workers than probes
	asyncLimit := params.AsyncLimit
	if estimatedProbes < asyncLimit {
		asyncLimit = estimatedProbes
	}

	probeFactor := time.Duration(math.Ceil(float64(estimatedProbes) / float64(asyncLimit)))
	portCount := len(params.ScanPorts)
	params.Logger.Debugf("total estimated network probes: %d, async limit: %d, probe timeout: %v, estimated time: min: %v max: %v typical: ~%v",
		estimatedProbes, asyncLimit, params.Timeout,
		probeFactor*params.Timeout,
		probeFactor*params.Timeout*time.Duration(portCount),
		probeFactor*params.Timeout*time.Duration(math.Min(float64(portCount), float64(params.MaxTimeoutsPerHost))))

	ipCh := make(chan uint32, asyncLimit)
	resultCh := make(chan []ProbeResult)

	wParams := workerParams{
		Params:   params,
		ipCh:     ipCh,
		resultCh: resultCh,
		ctx:      ctx,
		proto:    proto,
	}

	// start the workers before adding any ips so they are ready to process
	var wgIPWorkers sync.WaitGroup
	wgIPWorkers.Add(asyncLimit)
	for i := 0; i < asyncLimit; i++ {
		go func() {
			defer wgIPWorkers.Done()
			ipWorker(wParams)
		}()
	}

	go func() {
		var wgIPGenerators sync.WaitGroup
		for _, ipnet := range ipnets {
			select {
			case <-ctx.Done():
				// quit early if we have been cancelled
				return
			default:
			}

			// wait on each ipGenerator
			wgIPGenerators.Add(1)
			go func(inet *net.IPNet) {
				defer wgIPGenerators.Done()
				ipGenerator(ctx, inet, ipCh)
			}(ipnet)
		}

		// wait for all ip generators to finish, then we can close the ip channel
		wgIPGenerators.Wait()
		close(ipCh)

		// wait for the ipWorkers to finish, then close the results channel which
		// will let the enclosing function finish
		wgIPWorkers.Wait()
		close(resultCh)
	}()

	// this blocks until the resultCh is closed in above go routine
	return processResultChannel(resultCh, proto, params)
}

// processResultChannel reads all incoming results until the resultCh is closed.
// it determines if a device is new or existing, and proceeds accordingly.
//
// Does not check for context cancellation because we still want to
// process any in-flight results.
func processResultChannel(resultCh chan []ProbeResult, proto ProtocolSpecificDiscovery, params Params) []DiscoveredDevice {
	devices := make([]DiscoveredDevice, 0)
	for probeResults := range resultCh {
		if len(probeResults) == 0 {
			continue
		}

		for _, probeResult := range probeResults {
			dev, err := proto.ConvertProbeResult(probeResult, params)
			if err != nil {
				params.Logger.Warnf("issue converting probe result to discovered device: %s", err.Error())
				continue
			}
			devices = append(devices, dev)
		}
	}
	return devices
}

func handleConnectionInternal(host string, port string, conn net.Conn, params workerParams) {
	// on udp, the dial is always successful, so don't print
	if params.NetworkProtocol != NetworkUDP {
		// flag the host as up so that way we know we can scan additional ports (if configured)
		params.Logger.Debugf("Connection dialed %s://%s:%s", params.NetworkProtocol, host, port)
	}

	results, err := params.proto.OnConnectionDialed(host, port, conn, params.Params)
	if err != nil {
		params.Logger.Debugf(err.Error())
	} else if len(results) > 0 {
		params.resultCh <- results
	}
}

// probe attempts to make a connection to a specific ip and port list to determine
// if there is a service listening at that ip+port.
func probe(host string, ports []string, params workerParams) {
	port0 := ports[0]
	addr := host + ":" + port0

	params.Logger.Tracef("Dial: %s", addr)
	conn, err := net.DialTimeout(params.NetworkProtocol, addr, params.Timeout)
	if err != nil {
		params.Logger.Tracef(err.Error())
		// EHOSTUNREACH specifies that the host is un-reachable or there is no route to host. If this is
		// the case, do not bother probing the host any longer.
		// todo: should we quit on EHOSTDOWN as well?
		if errors.Is(err, syscall.EHOSTUNREACH) {
			// quit probing this host
			return
		}
		// all other errors will cause the scanning to continue to the other ports
	} else {
		defer conn.Close()
		handleConnectionInternal(host, port0, conn, params)
	}

	// if we are only scanning 1 port, nothing left to do
	if len(ports) == 1 {
		return
	}

	// scan the rest of the ports async (we know the host is at least reachable now)
	// the reason for this is that after the initial set of probes are completed, there is a lot less left
	var wg sync.WaitGroup
	for _, port := range ports[1:] {
		p := port
		addr := host + ":" + p
		params.Logger.Tracef("Dial: %s", addr)
		wg.Add(1)

		// wrap this code in a func in order to be able to defer the close method within
		// the for loop and not cause resource exhaustion.
		go func(p2 string) {
			defer wg.Done()

			conn2, err := net.DialTimeout(params.NetworkProtocol, addr, params.Timeout)
			if err != nil {
				params.Logger.Tracef(err.Error())
				return
			}
			defer conn2.Close()
			handleConnectionInternal(host, p2, conn2, params)
		}(p)
	}

	// wait for the rest of the probes
	wg.Wait()
}

// ipWorker pulls uint32s from the ipCh, convert to IPs, filters then ip
// to determine if a probe is to be made, makes the probe, and sends back successful
// probes to the resultCh.
func ipWorker(params workerParams) {
	ip := net.IP([]byte{0, 0, 0, 0})

	for {
		select {
		case <-params.ctx.Done():
			// stop working if we have been cancelled
			return

		case a, ok := <-params.ipCh:
			if !ok {
				// channel has been closed
				return
			}

			binary.BigEndian.PutUint32(ip, a)

			ipStr := ip.String()

			// filter out which ports to actually scan, and skip this host if no ports are returned
			ports := params.proto.ProbeFilter(ipStr, params.ScanPorts)
			if len(ports) == 0 {
				continue
			}

			probe(ipStr, ports, params)
		}
	}
}
