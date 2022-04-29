//
// Copyright (C) 2020 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"github.com/IOTechSystems/onvif"
	wsdiscovery "github.com/IOTechSystems/onvif/ws-discovery"
	"github.com/edgexfoundry/go-mod-core-contracts/v2/clients/logger"
	"github.com/gofrs/uuid"
	"github.com/pkg/errors"
	"io"
	"math"
	"math/bits"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	dsModels "github.com/edgexfoundry/device-sdk-go/v2/pkg/models"
	contract "github.com/edgexfoundry/go-mod-core-contracts/v2/models"
)

const (
	protocolName = "Onvif"
)

// discoveryInfo holds information about a discovered device
type discoveryInfo struct {
	deviceName string
	host       string
	port       string
	vendor     uint32
	model      uint32
	fwVersion  string
}

// newDiscoveryInfo creates a new discoveryInfo with just a host and port pre-filled
func newDiscoveryInfo(host, port string) *discoveryInfo {
	return &discoveryInfo{
		host: host,
		port: port,
	}
}

// workerParams is a helper struct to store shared parameters to ipWorkers
type workerParams struct {
	deviceMap map[string]contract.Device
	ipCh      <-chan uint32
	resultCh  chan<- []onvif.Device
	ctx       context.Context

	timeout   time.Duration
	scanPorts []string
}

type discoverParams struct {
	subnets                    []string
	asyncLimit                 int
	timeout                    time.Duration
	scanPorts                  []string
	defaultAuthMode            string
	defaultSecretPath          string
	multicastEthernetInterface string
	lc                         logger.LoggingClient
	driver                     *Driver
}

// computeNetSz computes the total amount of valid IP addresses for a given subnet size
// Subnets of size 31 and 32 have only 1 valid IP address
// Ex. For a /24 subnet, computeNetSz(24) -> 254
func computeNetSz(subnetSz int) uint32 {
	if subnetSz >= 31 {
		return 1
	}
	return ^uint32(0)>>subnetSz - 1
}

// autoDiscover probes all addresses in the configured network to attempt to discover any possible
// RFID readers that support LLRP.
func autoDiscover(ctx context.Context, params discoverParams) []dsModels.DiscoveredDevice {
	if len(params.subnets) == 0 {
		driver.lc.Warn("Discover was called, but no subnet information has been configured!")
		return nil
	}

	ipnets := make([]*net.IPNet, 0, len(params.subnets))
	var estimatedProbes int
	for _, cidr := range params.subnets {
		if cidr == "" {
			driver.lc.Warn("Empty CIDR provided, unable to scan for LLRP readers.")
			continue
		}

		ip, ipnet, err := net.ParseCIDR(cidr)
		if err != nil {
			driver.lc.Error(fmt.Sprintf("Unable to parse CIDR: %q", cidr), "error", err)
			continue
		}
		if ip == nil || ipnet == nil || ip.To4() == nil {
			driver.lc.Error("Currently only ipv4 subnets are supported.", "subnet", cidr)
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
	asyncLimit := params.asyncLimit
	if estimatedProbes < asyncLimit {
		asyncLimit = estimatedProbes
	}
	// todo: estimated probes take into account multiple scan ports?
	driver.lc.Debug(fmt.Sprintf("total estimated network probes: %d, async limit: %d, probe timeout: %v, total estimated time: %v",
		estimatedProbes, asyncLimit, params.timeout, time.Duration(math.Ceil(float64(estimatedProbes)/float64(asyncLimit)))*params.timeout*time.Duration(len(params.scanPorts))))

	ipCh := make(chan uint32, asyncLimit)
	resultCh := make(chan []onvif.Device)

	deviceMap := makeDeviceMap()
	wParams := workerParams{
		deviceMap: deviceMap,
		ipCh:      ipCh,
		resultCh:  resultCh,
		ctx:       ctx,
		timeout:   params.timeout,
		scanPorts: params.scanPorts,
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

		probeSOAP := wsdiscovery.BuildProbeMessage(uuid.Must(uuid.NewV4()).String(), nil, nil, map[string]string{"dn": "http://www.onvif.org/ver10/network/wsdl"})
		probeResponses := wsdiscovery.SendUDPMulticast(probeSOAP.String(), params.multicastEthernetInterface)
		devices, err := wsdiscovery.DevicesFromProbeResponses(probeResponses)
		if err == nil && len(devices) > 0 {
			resultCh <- devices
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
	return processResultChannel(resultCh, deviceMap, params)
}

// processResultChannel reads all incoming results until the resultCh is closed.
// it determines if a device is new or existing, and proceeds accordingly.
//
// Does not check for context cancellation because we still want to
// process any in-flight results.
func processResultChannel(resultCh chan []onvif.Device, deviceMap map[string]contract.Device, params discoverParams) []dsModels.DiscoveredDevice {
	discoveredDevices := make([]dsModels.DiscoveredDevice, 0)
	for onvifDevices := range resultCh {
		if onvifDevices == nil {
			continue
		}

		for _, onvifDevice := range onvifDevices {
			if onvifDevice.GetDeviceParams().EndpointRefAddress == "" {
				params.driver.lc.Warnf("The EndpointRefAddress is empty from the Onvif camera, unable to add the camera %s", onvifDevice.GetDeviceParams().Xaddr)
				continue
			}
			address, port := addressAndPort(onvifDevice.GetDeviceParams().Xaddr)
			dev := contract.Device{
				// Using Xaddr as the temporary name
				Name: onvifDevice.GetDeviceParams().Xaddr,
				Protocols: map[string]contract.ProtocolProperties{
					OnvifProtocol: {
						Address:    address,
						Port:       port,
						AuthMode:   params.defaultAuthMode,
						SecretPath: params.defaultSecretPath,
					},
				},
			}

			devInfo, edgexErr := params.driver.getDeviceInformation(dev)
			endpointRef := onvifDevice.GetDeviceParams().EndpointRefAddress
			var discovered dsModels.DiscoveredDevice
			if edgexErr != nil {
				params.driver.lc.Warnf("failed to get the device information for the camera %s, %v", endpointRef, edgexErr)
				dev.Protocols[OnvifProtocol][SecretPath] = endpointRef
				discovered = dsModels.DiscoveredDevice{
					Name:        endpointRef,
					Protocols:   dev.Protocols,
					Description: "Auto discovered Onvif camera",
					Labels:      []string{"auto-discovery"},
				}
				params.driver.lc.Debugf("Discovered unknown camera from the address '%s'", onvifDevice.GetDeviceParams().Xaddr)
			} else {
				dev.Protocols[OnvifProtocol][Manufacturer] = devInfo.Manufacturer
				dev.Protocols[OnvifProtocol][Model] = devInfo.Model
				dev.Protocols[OnvifProtocol][FirmwareVersion] = devInfo.FirmwareVersion
				dev.Protocols[OnvifProtocol][SerialNumber] = devInfo.SerialNumber
				dev.Protocols[OnvifProtocol][HardwareId] = devInfo.HardwareId

				// Spaces are not allowed in the device name
				deviceName := fmt.Sprintf("%s-%s-%s",
					strings.ReplaceAll(devInfo.Manufacturer, " ", "-"),
					strings.ReplaceAll(devInfo.Model, " ", "-"),
					onvifDevice.GetDeviceParams().EndpointRefAddress)

				discovered = dsModels.DiscoveredDevice{
					Name:        deviceName,
					Protocols:   dev.Protocols,
					Description: fmt.Sprintf("%s %s Camera", devInfo.Manufacturer, devInfo.Model),
					Labels:      []string{"auto-discovery", devInfo.Manufacturer, devInfo.Model},
				}
				params.driver.lc.Debugf("Discovered camera from the address '%s'", onvifDevice.GetDeviceParams().Xaddr)
			}
			discoveredDevices = append(discoveredDevices, discovered)
		}

		//// check if any devices already exist at that address, and if so disable them
		//existing, found := deviceMap[info.host+":"+info.port]
		//if found && existing.Name != info.deviceName {
		//	// disable it and remove its protocol information since it is no longer valid
		//	delete(existing.Protocols, "tcp")
		//	existing.OperatingState = contract.Down
		//	if err := driver.svc.UpdateDevice(existing); err != nil {
		//		driver.lc.Warn("There was an issue trying to disable an existing device.",
		//			"deviceName", existing.Name,
		//			"error", err)
		//	}
		//}
		//
		//// check if we have an existing device registered with this name
		//device, err := driver.svc.GetDeviceByName(info.deviceName)
		//if err != nil {
		//	// no existing device; add it to the list and move on
		//	discovered = append(discovered, newDiscoveredDevice(info))
		//	continue
		//}
		//
		//// this means we have discovered an existing device that is
		//// either disabled or has changed IP addresses.
		//// we need to update its protocol information and operating state
		//if err := info.updateExistingDevice(device); err != nil {
		//	driver.lc.Warn("There was an issue trying to update an existing device based on newly discovered details.",
		//		"deviceName", device.Name,
		//		"discoveryInfo", fmt.Sprintf("%+v", info),
		//		"error", err)
		//}
	}
	return discoveredDevices
}

// updateExistingDevice is used when an existing device is discovered
// and needs to update its information to either a new address or set
// its operating state to enabled.
func (info *discoveryInfo) updateExistingDevice(device contract.Device) error {
	shouldUpdate := false
	if device.OperatingState == contract.Down {
		device.OperatingState = contract.Up
		shouldUpdate = true
	}

	tcpInfo := device.Protocols["tcp"]
	if tcpInfo == nil ||
		info.host != tcpInfo["host"] ||
		info.port != tcpInfo["port"] {
		driver.lc.Info("Existing device has been discovered with a different network address.",
			"oldInfo", fmt.Sprintf("%+v", tcpInfo),
			"discoveredInfo", fmt.Sprintf("%+v", info))

		device.Protocols["tcp"] = map[string]string{
			"host": info.host,
			"port": info.port,
		}
		// make sure it is enabled
		device.OperatingState = contract.Up
		shouldUpdate = true
	}

	if !shouldUpdate {
		// the address is the same and device is already enabled, should not reach here
		driver.lc.Warn("Re-discovered existing device at the same TCP address, nothing to do.")
		return nil
	}

	if err := driver.svc.UpdateDevice(device); err != nil {
		driver.lc.Error("There was an error updating the tcp address for an existing device.",
			"deviceName", device.Name,
			"error", err)
		return err
	}

	return nil
}

// makeDeviceMap creates a lookup table of existing devices by tcp address in order to skip scanning
func makeDeviceMap() map[string]contract.Device {
	devices := driver.svc.Devices()
	deviceMap := make(map[string]contract.Device, len(devices))

	for _, d := range devices {
		onvifInfo := d.Protocols[protocolName]
		if onvifInfo == nil {
			driver.lc.Warnf("Found registered device %s without %s protocol information.", d.Name, protocolName)
			continue
		}

		host, port := onvifInfo["Address"], onvifInfo["Port"]
		if host == "" || port == "" {
			driver.lc.Warnf("Registered device is missing required %s protocol information. Address: %v, Port: %v",
				protocolName, host, port)
			continue
		}

		deviceMap[host+":"+port] = d
	}

	return deviceMap
}

// ipGenerator generates all valid IP addresses for a given subnet, and
// sends them to the ip channel one at a time
func ipGenerator(ctx context.Context, inet *net.IPNet, ipCh chan<- uint32) {
	addr := inet.IP.To4()
	if addr == nil {
		return
	}

	mask := inet.Mask
	if len(mask) == net.IPv6len {
		mask = mask[12:]
	} else if len(mask) != net.IPv4len {
		return
	}

	umask := binary.BigEndian.Uint32(mask)
	maskSz := bits.OnesCount32(umask)
	if maskSz <= 1 {
		return // skip point-to-point connections
	} else if maskSz >= 31 {
		ipCh <- binary.BigEndian.Uint32(inet.IP)
		return
	}

	netId := binary.BigEndian.Uint32(addr) & umask // network ID
	bcast := netId ^ (^umask)
	for ip := netId + 1; ip < bcast; ip++ {
		if netId&umask != ip&umask {
			continue
		}

		select {
		case <-ctx.Done():
			// bail if we have been cancelled
			return
		case ipCh <- ip:
		}
	}
}

// probe attempts to make a connection to a specific ip and port to determine
// if an LLRP reader exists at that network address
func probe(host, port string, timeout time.Duration) ([]onvif.Device, error) {
	addr := host + ":" + port
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, err
	}
	//defer conn.Close()

	driver.lc.Info("Connection dialed", "host", host, "port", port)
	conn.Close()

	dev, err := onvif.NewDevice(onvif.DeviceParams{
		Xaddr: fmt.Sprintf("%s:%s", host, port),
		HttpClient: &http.Client{
			Timeout: time.Duration(5) * time.Second,
		},
	})
	res, err := dev.CallOnvifFunction(onvif.DeviceWebService, onvif.GetSystemDateAndTime, nil)
	js, err := json.Marshal(res)
	driver.lc.Infof("Got Res: %v", string(js))

	res, err = dev.CallOnvifFunction(onvif.DeviceWebService, onvif.GetHostname, nil)
	js, err = json.Marshal(res)
	driver.lc.Infof("Got Res2: %v", string(js))

	return nil, nil

	probeSOAP := wsdiscovery.BuildProbeMessage(uuid.Must(uuid.NewV4()).String(), nil, nil, map[string]string{"dn": "http://www.onvif.org/ver10/network/wsdl"})

	if _, err = conn.Write([]byte(probeSOAP.String())); err != nil {
		err = errors.Wrap(err, "failed to write probe message")
		fmt.Println(err.Error())
		return nil, err
	}

	if err = conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		err = errors.Wrap(err, "failed to set read deadline")
		fmt.Println(err.Error())
		return nil, err
	}

	buf := make([]byte, 5)
	if _, err = io.ReadFull(conn, buf); err != nil {
		err = errors.Wrap(err, "failed to read header")
		fmt.Println(err.Error())
		return nil, err
	}
	if string(buf) != "<?xml" {
		fmt.Println("Non Xml response recieved")
		return nil, nil
	}

	fmt.Println("Got Xml")

	buf2, err := io.ReadAll(conn)
	if err != nil {
		return nil, err
	}
	fmt.Printf("\nGot Bytes: %s\n", string(buf2))

	response := string(buf) + string(buf2)
	devices, err := wsdiscovery.DevicesFromProbeResponses([]string{response})
	if err != nil {
		return nil, err
	}
	if len(devices) == 0 {
		fmt.Println("No devices matched")
		return nil, nil
	}

	return devices, nil
}

// ipWorker pulls uint32s, convert to IPs, and sends back successful probes to the resultCh
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

			for _, scanPort := range params.scanPorts {
				//addr := ipStr + ":" + scanPort
				//if d, found := params.deviceMap[addr]; found {
				//	if d.OperatingState == contract.Up {
				//		driver.lc.Debug("Skip scan of " + addr + ", device already registered.")
				//		continue
				//	}
				//	driver.lc.Info("Existing device in disabled (disconnected) state will be scanned again.",
				//		"address", addr,
				//		"deviceName", d.Name)
				//}

				select {
				case <-params.ctx.Done():
					// bail if we have already been cancelled
					return
				default:
				}

				if info, err := probe(ipStr, scanPort, params.timeout); err == nil && info != nil {
					params.resultCh <- info
				}
			}
		}
	}
}