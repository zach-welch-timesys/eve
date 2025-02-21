// Copyright (c) 2019 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

// Probe to the local interface nexthop and remote servers

package zedrouter

import (
	"crypto/tls"
	"fmt"
	"io/ioutil"
	"net"
	"strings"
	"time"

	"github.com/lf-edge/eve/pkg/pillar/cast"
	"github.com/lf-edge/eve/pkg/pillar/types"
	"github.com/lf-edge/eve/pkg/pillar/zedcloud"
	log "github.com/sirupsen/logrus"
	fastping "github.com/tatsushid/go-fastping"
)

const (
	maxContFailCnt     uint32 = 4   // number of continuous failure to declare Down
	maxContSuccessCnt  uint32 = 3   // number of continuous success to declare UP
	maxPingWait        int    = 100 // wait for 100 millisecond for ping timeout
	maxRemoteProbeWait uint32 = 3   // wait for 3 seconds for remote host response
	remoteTolocalRatio uint32 = 10  // every 10 times of local ping, perform remote probing
	minProbeRatio      uint32 = 5   // user defined ratio of local/remote min will be set to 5
	// e.g. if the local ping timer is every 15 seconds, every remote httping is every 2.5 minutes
	serverFileName   string = "/config/server"
	nhProbeInterval  uint32 = 15                    // probe interval
	stayDownMinCount uint32 = 600 / nhProbeInterval // at least stay down for 10 min
	stayUPMinCount   uint32 = stayDownMinCount
)

// intfTypeXXX - can be changed to more generic in terms of probing stages
const (
	intfTypeFree int = iota + 1
	intfTypeNonFree
)

type probeRes struct {
	isRemoteReply bool
	latency       int64 // in millisecond
}

var iteration uint32
var serverNameAndPort string
var addrChgTime time.Time // record last time intf address change time
var zcloudCtx = zedcloud.ZedCloudContext{
	FailureFunc:        zedcloud.ZedCloudFailure,
	SuccessFunc:        zedcloud.ZedCloudSuccess,
	TlsConfig:          &tls.Config{InsecureSkipVerify: true},
	NetworkSendTimeout: maxRemoteProbeWait,
}

// called from handleDNSModify
func deviceUpdateNIprobing(ctx *zedrouterContext, status *types.DeviceNetworkStatus) {
	var needTrigPing bool
	pub := ctx.pubNetworkInstanceStatus
	log.Debugf("deviceUpdateNIprobing: enter\n")
	for _, port := range status.Ports {
		log.Infof("deviceUpdateNIprobing: port %s\n", port.Name)
		if !port.IsMgmt { // for now, only probing the uplink
			continue
		}

		items := pub.GetAll()
		for _, st := range items {
			netstatus := cast.CastNetworkInstanceStatus(st)
			if !isSharedPortLabel(netstatus.Port) {
				continue
			}
			if niProbingUpdatePort(ctx, port, &netstatus) {
				needTrigPing = true
			}
			checkNIprobeUplink(ctx, &netstatus)
		}
	}
	if needTrigPing {
		setProbeTimer(ctx, 1) // trigger probe timer faster (in 1 sec)
	}
}

// called from handleNetworkInstanceModify and handleNetworkInstanceCreate
func niUpdateNIprobing(ctx *zedrouterContext, status *types.NetworkInstanceStatus) {
	pub := ctx.subDeviceNetworkStatus
	items := pub.GetAll()
	portList := getIfNameListForPort(ctx, status.Port)
	log.Infof("niUpdateNIprobing: enter, number of ports %d\n", len(portList))
	for _, st := range items {
		devStatus := cast.CastDeviceNetworkStatus(st)

		for _, port := range portList {
			devPort := getDevPort(&devStatus, port)
			if devPort == nil {
				continue
			}
			if !isSharedPortLabel(status.Port) &&
				status.Port != devPort.Name {
				continue
			}
			niProbingUpdatePort(ctx, *devPort, status)
		}
	}
	checkNIprobeUplink(ctx, status)
}

func getDevPort(status *types.DeviceNetworkStatus, port string) *types.NetworkPortStatus {
	for _, tmpport := range status.Ports {
		if strings.Compare(tmpport.Name, port) == 0 {
			return &tmpport
		}
	}
	return nil
}

func niProbingUpdatePort(ctx *zedrouterContext, port types.NetworkPortStatus,
	netstatus *types.NetworkInstanceStatus) bool {
	var needTrigPing bool
	log.Debugf("niProbingUpdatePort: %s enter\n", netstatus.BridgeName)
	if netstatus.Error != "" {
		log.Errorf("niProbingUpdatePort: Network instance is in errored state: %s",
			netstatus.Error)
		return needTrigPing
	}
	if _, ok := netstatus.PInfo[port.IfName]; !ok {
		if port.IfName == "" { // no need to probe for air-gap type of NI
			return needTrigPing
		}
		info := types.ProbeInfo{
			IfName:       port.IfName,
			IsPresent:    true,
			GatewayUP:    true,
			NhAddr:       port.Gateway,
			LocalAddr:    portGetIntfAddr(port),
			IsFree:       port.Free,
			RemoteHostUP: true,
		}
		netstatus.PInfo[port.IfName] = info
		log.Infof("niProbingUpdatePort: %s assigned new %s, info len %d, isFree %v\n",
			netstatus.BridgeName, port.IfName, len(netstatus.PInfo), info.IsFree)
	} else {
		info := netstatus.PInfo[port.IfName]
		prevLocalAddr := info.LocalAddr
		info.IsPresent = true
		info.NhAddr = port.Gateway
		info.LocalAddr = portGetIntfAddr(port)
		info.IsFree = port.Free
		// the probe status are copied inside publish NI status
		netstatus.PInfo[port.IfName] = info
		log.Infof("niProbingUpdatePort: %s modified %s, isFree %v\n", netstatus.BridgeName, port.IfName, info.IsFree)
		if netstatus.Port == port.Name {
			// if the intf lose ip address or gain ip address, react faster
			// XXX detect changes to LocalAddr and NHAddr in general?
			if prevLocalAddr != nil && !prevLocalAddr.IsUnspecified() &&
				(info.LocalAddr == nil || info.LocalAddr.IsUnspecified()) {
				log.Infof("niProbingUpdatePort: %s lose addr modified %s, addrlen %d, addr %v, nh %v",
					netstatus.BridgeName, port.IfName, len(port.AddrInfoList), info.LocalAddr, info.NhAddr)
				needTrigPing = true
			} else if (prevLocalAddr == nil || prevLocalAddr.IsUnspecified()) &&
				info.LocalAddr != nil && !info.LocalAddr.IsUnspecified() {
				log.Infof("niProbingUpdatePort: %s gain addr modified %s, addr %v, nh %v",
					netstatus.BridgeName, port.IfName, info.LocalAddr, info.NhAddr)
				needTrigPing = true
			}
		}
	}
	publishNetworkInstanceStatus(ctx, netstatus)
	if needTrigPing {
		elapsed := time.Since(addrChgTime).Seconds()
		// to prevent the loose cable is constantly flapping the UP/DOWN, wait at least 10 min
		if elapsed > 600 {
			addrChgTime = time.Now()
		} else {
			needTrigPing = false
		}
	}
	return needTrigPing
}

// after port or NI changes, if we don't have a current uplink,
// randomly assign one and publish, if we already do, leave it as is
// each NI may have a different good uplink
func checkNIprobeUplink(ctx *zedrouterContext, status *types.NetworkInstanceStatus) {
	// find and remove the stale info since the port has been removed
	for _, info := range status.PInfo {
		if !info.IsPresent {
			if _, ok := status.PInfo[info.IfName]; ok {
				delete(status.PInfo, info.IfName)
			}
		} else {
			info.IsPresent = false
		}
	}

	if status.CurrentUplinkIntf != "" {
		if _, ok := status.PInfo[status.CurrentUplinkIntf]; ok {
			if strings.Compare(status.PInfo[status.CurrentUplinkIntf].IfName, status.CurrentUplinkIntf) == 0 {
				return
			}
		}
		// if the Current Uplink intf does not have an info entry, re-pick one below
		status.CurrentUplinkIntf = ""
	}

	if len(status.PInfo) > 0 {
		// Try and find an interface that has unicast IP address.
		// No link local.
		for _, info := range status.PInfo {
			// Pick uplink with atleast one usable IP address
			ifNameList := getIfNameListForPort(ctx, info.IfName)
			if len(ifNameList) != 0 {
				for _, ifName := range ifNameList {
					_, err := types.GetLocalAddrAnyNoLinkLocal(*ctx.deviceNetworkStatus, 0, ifName)
					if err != nil {
						continue
					}
					status.CurrentUplinkIntf = info.IfName
					log.Infof("checkNIprobeUplink: bridge %s pick %s as uplink\n",
						status.BridgeName, info.IfName)
					break
				}
			}
			if status.CurrentUplinkIntf != "" {
				break
			}
		}
		if status.CurrentUplinkIntf == "" {
			// We are not able to find a port with usable unicast IP address.
			// Try and find a port that atleast has a local UP address.
			for _, info := range status.PInfo {
				ifNameList := getIfNameListForPort(ctx, info.IfName)
				if len(ifNameList) != 0 {
					for _, ifName := range ifNameList {
						_, err := types.GetLocalAddrAny(*ctx.deviceNetworkStatus, 0, ifName)
						if err != nil {
							continue
						}
						status.CurrentUplinkIntf = info.IfName
						log.Infof("checkNIprobeUplink: bridge %s pick %s as uplink\n",
							status.BridgeName, info.IfName)
						break
					}
				}
				if status.CurrentUplinkIntf != "" {
					break
				}
			}
		}
		// If none of the interfaces have valid unicast/local IP addresss just pick the first
		if status.CurrentUplinkIntf == "" {
			if len(status.PInfo) > 0 {
				var port string
				for port = range status.PInfo {
					break
				}
				info := status.PInfo[port]
				status.CurrentUplinkIntf = info.IfName
				log.Infof("checkNIprobeUplink: bridge %s pick %s as uplink\n",
					status.BridgeName, info.IfName)
			}
		}
		publishNetworkInstanceStatus(ctx, status)
	}
}

func portGetIntfAddr(port types.NetworkPortStatus) net.IP {
	var localip net.IP
	for _, addrinfo := range port.AddrInfoList {
		if port.Subnet.Contains(addrinfo.Addr) {
			localip = addrinfo.Addr
		}
	}
	return localip
}

// a go routine driven by the HostProbeTimer in zedrouter, to perform the
// local and remote(less frequent) host probing
func launchHostProbe(ctx *zedrouterContext) {
	var isReachable, needSendSignal, bringIntfDown bool
	var remoteURL string
	nhPing := make(map[string]bool)
	localDown := make(map[string]bool)
	remoteProbe := make(map[string]map[string]probeRes)
	log.Infof("launchHostProbe: enter\n")
	dpub := ctx.subDeviceNetworkStatus
	ditems := dpub.GetAll()

	if serverNameAndPort == "" {
		server, err := ioutil.ReadFile(serverFileName)
		if err == nil {
			serverNameAndPort = strings.TrimSpace(string(server))
		}
	}
	if serverNameAndPort != "" {
		remoteURL = serverNameAndPort
	} else {
		remoteURL = "www.google.com"
	}

	for _, netstatus := range ctx.networkInstanceStatusMap {
		var anyNIStateChg bool
		// XXX Revisit when we support other network instance types.
		if netstatus.Type != types.NetworkInstanceTypeLocal &&
			netstatus.Type != types.NetworkInstanceTypeCloud {
			continue
		}
		log.Debugf("launchHostProbe: ni(%s) current uplink %s, isUP %v, prev %s, update %v\n",
			netstatus.BridgeName, netstatus.CurrentUplinkIntf, netstatus.CurrIntfUP, netstatus.PrevUplinkIntf, netstatus.NeedIntfUpdate)
		netstatus.NeedIntfUpdate = false

		for _, info := range netstatus.PInfo {
			var needToProbe bool
			var isRemoteResp probeRes
			log.Debugf("launchHostProbe: intf %s, gw %v, statusUP %v, remoteHostUP %v\n",
				info.IfName, info.NhAddr, info.GatewayUP, info.RemoteHostUP)

			// Local nexthop ping, only apply to Ethernet type of interface
			if info.IsFree {
				if _, ok := nhPing[info.IfName]; !ok {
					isReachable, bringIntfDown = probeFastPing(info)
					nhPing[info.IfName] = isReachable
					localDown[info.IfName] = bringIntfDown
				} else {
					isReachable = nhPing[info.IfName]
					bringIntfDown = localDown[info.IfName]
					log.Debugf("launchHostProbe: already got ping result on %s(%s) %v\n", info.IfName, info.NhAddr.String(), isReachable)
				}

				if bringIntfDown {
					log.Debugf("launchHostProbe: %s local address lost, bring it down/down\n", info.IfName)
					info.GatewayUP = false
					info.RemoteHostUP = false
				}
				if probeProcessReply(&info, isReachable, 0, true) {
					anyNIStateChg = true
				}
				log.Debugf("launchHostProbe(%d): gateway up %v, success count %d, failed count %d, remote success %d, remote fail %d\n",
					iteration, info.GatewayUP, info.SuccessCnt, info.FailedCnt, info.SuccessProbeCnt, info.FailedProbeCnt)
			}

			// for every X number of nexthop ping iteration, do the remote probing
			// although we could have checked if the nexthop is down, there is no need
			// to do remote probing, but just in case, local nexthop ping is filtered by
			// the gateway firewall, and since we only do X iteration, it's simpler just doing it
			if iteration%getProbeRatio(netstatus) == 0 {
				// get user specified url/ip
				remoteURL = getRemoteURL(netstatus, remoteURL)

				// probing remote host
				tmpRes := remoteProbe[info.IfName]
				if tmpRes == nil {
					tmpRes := make(map[string]probeRes)
					remoteProbe[info.IfName] = tmpRes
				}
				// if has already been done for this intf/remoteURL of this session, then
				// copy the result over to other NIs
				if _, ok := remoteProbe[info.IfName][remoteURL]; !ok {
					needToProbe = true
				} else {
					isRemoteResp = remoteProbe[info.IfName][remoteURL]

					log.Debugf("launchHostProbe: probe on %s to remote %s, resp %v\n", info.IfName, remoteURL, isRemoteResp)
				}

				if needToProbe {
					var foundport bool
					for _, st := range ditems {
						devStatus := cast.CastDeviceNetworkStatus(st)
						for _, port := range devStatus.Ports {
							if strings.Compare(port.IfName, info.IfName) == 0 {
								zcloudCtx.DeviceNetworkStatus = &devStatus
								foundport = true
								break
							}
						}
						if foundport {
							break
						}
					}
					if foundport {
						startTime := time.Now()
						resp, _, rtf, err := zedcloud.SendOnIntf(zcloudCtx, remoteURL, info.IfName, 0, nil, true)
						if err != nil {
							log.Debugf("launchHostProbe: send on intf %s, err %v\n", info.IfName, err)
						}
						if rtf {
							log.Debugf("launchHostProbe: remote temp failure\n")
						}
						if resp != nil {
							log.Debugf("launchHostProbe: server %s status code %d\n", serverNameAndPort, resp.StatusCode)
							//
							// isRemoteResp.isRemoteReply = (resp.StatusCode == 200)
							// make it any reply is good
							isRemoteResp.isRemoteReply = true
						}
						isRemoteResp.latency = time.Since(startTime).Nanoseconds() / int64(time.Millisecond)
					}
					remoteProbe[info.IfName][remoteURL] = isRemoteResp
				}

				if probeProcessReply(&info, isRemoteResp.isRemoteReply, isRemoteResp.latency, false) {
					anyNIStateChg = true
				}
				log.Debugf("launchHostProbe: probe on %s to remote %s, latency %d msec, success cnt %d, failed cnt %d, need probe %v\n",
					info.IfName, remoteURL, isRemoteResp.latency, info.SuccessProbeCnt, info.FailedProbeCnt, needToProbe)
			}

			netstatus.PInfo[info.IfName] = info
		}
		probeCheckStatus(netstatus)
		// we need to trigger the change at least once at start to set the initial Uplink intf
		if netstatus.NeedIntfUpdate || netstatus.TriggerCnt == 0 {
			needSendSignal = true
			netstatus.TriggerCnt++
		}
		if anyNIStateChg || needSendSignal { // one of the uplink has local/remote state change regardless of CurrUPlinkIntf change, publish
			log.Debugf("launchHostProbe: send NI status update\n")
			publishNetworkInstanceStatus(ctx, netstatus)
		}
	}
	if needSendSignal {
		log.Debugf("launchHostProbe: send uplink signal\n")
		ctx.checkNIUplinks <- true
	}
	iteration++
	setProbeTimer(ctx, nhProbeInterval)
}

func probeCheckStatus(status *types.NetworkInstanceStatus) {
	if len(status.PInfo) == 0 {
		return
	}

	prevIntf := status.CurrentUplinkIntf // the old Curr
	f := make([]bool, 3)                 // non-valid, free, non-free
	f[intfTypeFree] = true
	f[intfTypeNonFree] = false
	// check probe stats from Free pool first
	// continue to the non-Free if there is no available good link in the Free pool
	for c := intfTypeFree; c <= intfTypeNonFree; c++ {
		var numOfUps int
		probeCheckStatusUseType(status, f[c])
		currIntf := status.CurrentUplinkIntf
		if currIntf != "" {
			if currinfo, ok := status.PInfo[currIntf]; ok {
				numOfUps = infoUpCount(currinfo)
				log.Infof("probeCheckStatus: level %d, currintf %s, num Ups %d\n", c, currIntf, numOfUps)
			}
		}
		if numOfUps > 0 {
			break
		}
	}
	if strings.Compare(status.CurrentUplinkIntf, prevIntf) != 0 { // the new Curr comparing to old Curr
		log.Debugf("probeCheckStatus: changing from %s to %s\n",
			status.PrevUplinkIntf, status.CurrentUplinkIntf)
		status.PrevUplinkIntf = prevIntf
		status.NeedIntfUpdate = true
		stateUP, err := getCurrIntfState(status, status.CurrentUplinkIntf)
		if err == nil {
			status.CurrIntfUP = stateUP
		}
		log.Debugf("probeCheckStatus: changing from %s to %s, intfup %v\n",
			status.PrevUplinkIntf, status.CurrentUplinkIntf, status.CurrIntfUP)
	} else { // even if the Curr intf does not change, it can transit state
		stateUP, err := getCurrIntfState(status, status.CurrentUplinkIntf)
		if err == nil {
			if status.CurrIntfUP != stateUP {
				log.Debugf("probeCheckStatus: intf %s state from %v to %v\n", prevIntf, status.CurrIntfUP, stateUP)
				status.CurrIntfUP = stateUP
				status.NeedIntfUpdate = true
			}
		}
	}
	log.Debugf("probeCheckStatus: %s current Uplink Intf %s, prev %s, need-update %v\n",
		status.BridgeName, status.CurrentUplinkIntf, status.PrevUplinkIntf, status.NeedIntfUpdate)
}

// How to determine the time to switch to another interface
// -- compare only within the same interface class: ether, lte, sat
// -- Random assign one intf intially
// -- each intf has 3 types of states: both local and remote report UP, only one is UP, both are Down
// -- try to pick and switch to the one has the highest degree of UPs
// -- otherwise, don't switch
func probeCheckStatusUseType(status *types.NetworkInstanceStatus, free bool) {
	var numOfUps, upCnt int
	currIntf := status.CurrentUplinkIntf
	log.Debugf("probeCheckStatusUseType: from %s, free %v, curr intf %s\n", status.BridgeName, free, currIntf)
	if currIntf == "" { // if we don't have a Curr, get one from the same level free/non-free
		for _, info := range status.PInfo {
			if free != info.IsFree {
				continue
			}
			log.Debugf("probeCheckStatusUseType: currintf null, randomly assign %s now\n", info.IfName)
			currIntf = info.IfName
		}
	}
	if currIntf != "" {
		if _, ok := status.PInfo[currIntf]; !ok {
			// should not happen, zero it out, come next time to process
			log.Errorf("probeCheckStatusUseType: current Uplink Intf %s error", currIntf)
			status.CurrentUplinkIntf = ""
			return
		}
		// when we have more than two cost levels, this need to change to compare numbers
		// if the current intf is non-free, and we are in free loop, to see if we can get free UP intf
		if !status.PInfo[currIntf].IsFree && free {
			for _, info := range status.PInfo {
				if free != info.IsFree {
					continue
				}
				if infoUpCount(info) == 0 {
					continue
				}
				log.Debugf("probeCheckStatusUseType: currintf is non-free, randomly assign %s now\n", info.IfName)
				currIntf = info.IfName
				break
			}
		}
		currinfo := status.PInfo[currIntf]
		numOfUps = infoUpCount(currinfo)
		log.Debugf("probeCheckStatusUseType: curr intf %s, num ups %d\n", currIntf, numOfUps)
		if free == currinfo.IsFree && numOfUps == 2 { // good, no need to change
			status.CurrentUplinkIntf = currIntf
			return
		}
		log.Debugf("probeCheckStatusUseType: before loop\n")
		for _, info := range status.PInfo {
			if free != info.IsFree {
				continue
			}
			log.Debugf("probeCheckStatusUseType: compare %s, and %s\n", info.IfName, currIntf)
			if strings.Compare(info.IfName, currIntf) == 0 {
				continue
			}
			upCnt = infoUpCount(info)
			log.Debugf("probeCheckStatusUseType: upcnt %d, vs my ups %d\n", upCnt, numOfUps)
			if numOfUps < upCnt {
				currIntf = info.IfName
				numOfUps = upCnt
			}
		}
	}
	// We did not find any viable port, do not overwrite the current selected port
	if currIntf != "" {
		status.CurrentUplinkIntf = currIntf
	}
}

func getCurrIntfState(status *types.NetworkInstanceStatus, currIntf string) (types.CurrIntfStatusType, error) {
	if _, ok := status.PInfo[currIntf]; !ok {
		err := fmt.Errorf("getCurrIntfState: intf %s has no info", currIntf)
		log.Errorf("getCurrIntfState: %s, error %v\n", currIntf, err)
		return types.CurrIntfNone, err
	}
	info := status.PInfo[currIntf]
	if infoUpCount(info) > 0 {
		return types.CurrIntfUP, nil
	} else {
		return types.CurrIntfDown, nil
	}
}

func infoUpCount(info types.ProbeInfo) int {
	var upCnt int
	if info.GatewayUP && info.RemoteHostUP {
		upCnt = 2
	} else if info.GatewayUP || info.RemoteHostUP {
		upCnt = 1
	}
	return upCnt
}

func getRemoteURL(netstatus *types.NetworkInstanceStatus, defaultURL string) string {
	remoteURL := defaultURL
	// check on User defined URL/IP address
	if netstatus.PConfig.ServerURL != "" {
		if strings.Contains(netstatus.PConfig.ServerURL, "http") {
			remoteURL = netstatus.PConfig.ServerURL
		} else {
			// use 'http' instead of 'https'
			remoteURL = "http://" + netstatus.PConfig.ServerURL
		}
	} else if netstatus.PConfig.ServerIP != nil && !netstatus.PConfig.ServerIP.IsUnspecified() {
		remoteURL = "http://" + netstatus.PConfig.ServerIP.String()
	}
	return remoteURL
}

func getProbeRatio(netstatus *types.NetworkInstanceStatus) uint32 {
	if netstatus.PConfig.ProbeInterval != 0 {
		ratio := netstatus.PConfig.ProbeInterval / nhProbeInterval
		if ratio < minProbeRatio {
			return minProbeRatio
		}
		return ratio
	}
	return remoteTolocalRatio
}

// base on the probe result, determine if the port should be good to use
// and record the latency data for reaching the remote host
func probeProcessReply(info *types.ProbeInfo, gotReply bool, latency int64, isLocal bool) bool {
	var stateChange bool
	if isLocal {
		log.Infof("probeProcessReply: intf %s, gw up %v, sucess count %d, down count %d, got reply %v\n",
			info.IfName, info.GatewayUP, info.SuccessCnt, info.FailedCnt, gotReply)
		if gotReply {
			// fast convergence treatment for local ping, if the intf has stayed down for a while
			// and not a flapping case, when on this first ping success, bring the local GatewayUP to 'UP'
			if !info.GatewayUP && info.SuccessCnt == 0 && info.FailedCnt > stayDownMinCount {
				info.GatewayUP = true
				stateChange = true
				log.Debugf("probeProcessReply: intf %s, down count %d, ping success, bring it up\n",
					info.IfName, info.FailedCnt)
			}
			info.SuccessCnt++
			info.FailedCnt = 0
			info.TransDown = false
		} else {
			if info.GatewayUP && info.FailedCnt == 0 && info.SuccessCnt > stayUPMinCount {
				info.TransDown = true
			}
			info.FailedCnt++
			info.SuccessCnt = 0
		}
		if info.FailedCnt > maxContFailCnt && info.GatewayUP {
			info.GatewayUP = false
			stateChange = true
		} else if info.SuccessCnt > maxContSuccessCnt && !info.GatewayUP {
			info.GatewayUP = true
			stateChange = true
		}
	} else {
		log.Debugf("probeProcessReply: intf %s, remote probing got reply %v, success count %d, fail count %d\n",
			info.IfName, gotReply, info.SuccessProbeCnt, info.FailedProbeCnt)
		if gotReply {
			totalLatency := info.AveLatency * int64(info.SuccessProbeCnt)
			info.SuccessProbeCnt++
			info.AveLatency = (totalLatency + latency) / int64(info.SuccessProbeCnt)
			info.FailedProbeCnt = 0
		} else {
			// if remote probe success for a while, and local ping transition from up->down,
			// bring the remote down now
			// it can happen the first remote probe fails, but local ping has not bright down the Gateway,
			//
			if info.TransDown && !info.GatewayUP && info.RemoteHostUP && info.FailedProbeCnt < 2 {
				info.RemoteHostUP = false
				stateChange = true
				info.TransDown = false
			}
			info.FailedProbeCnt++
			info.SuccessProbeCnt = 0
			info.AveLatency = 0
		}
		if info.FailedProbeCnt > maxContFailCnt && info.RemoteHostUP {
			info.RemoteHostUP = false
			stateChange = true
		} else if info.SuccessProbeCnt > maxContSuccessCnt && !info.RemoteHostUP {
			info.RemoteHostUP = true
			stateChange = true
		}
	}
	return stateChange
}

func probeFastPing(info types.ProbeInfo) (bool, bool) {
	var dstaddress, srcaddress net.IPAddr
	var pingSuccess bool
	p := fastping.NewPinger()

	if info.LocalAddr == nil || info.LocalAddr.IsUnspecified() {
		if info.GatewayUP || info.RemoteHostUP {
			return false, true
		}
		return false, false
	}

	// if we don't have a gateway address or local intf address, no need to ping
	if info.NhAddr.IsUnspecified() {
		return false, false
	}

	dstaddress.IP = info.NhAddr
	p.AddIPAddr(&dstaddress)

	srcaddress.IP = info.LocalAddr
	p.Source(srcaddress.String())
	if srcaddress.String() == "" || dstaddress.String() == "" {
		return false, false
	}
	p.MaxRTT = time.Millisecond * time.Duration(maxPingWait)
	log.Debugf("probeFastPing: add to ping, address %s with source %s, maxrtt %v\n",
		dstaddress.String(), srcaddress.String(), p.MaxRTT)
	p.OnRecv = func(ip *net.IPAddr, d time.Duration) {
		if strings.Compare(ip.String(), dstaddress.String()) == 0 {
			pingSuccess = true
			log.Debugf("probeFastPing: got reply from %s, duration %d nanosec or rtt %v\n",
				dstaddress.String(), int64(d.Nanoseconds()), d)
		}
	}
	p.OnIdle = func() {
		log.Debugf("probeFastPing: run finish\n")
	}
	err := p.Run()
	if err != nil {
		log.Debugf("probeFastPing: run error, %v\n", err)
	}
	return pingSuccess, false
}

func setProbeTimer(ctx *zedrouterContext, probeIntv uint32) {
	interval := time.Duration(probeIntv)
	log.Debugf("setProbeTimer: interval %d sec\n", interval)
	ctx.hostProbeTimer = time.NewTimer(interval * time.Second)
}

// copy probing stats from the NI status ListMap into status
func copyProbeStats(ctx *zedrouterContext, netstatus *types.NetworkInstanceStatus) {
	mapstatus := ctx.networkInstanceStatusMap[netstatus.UUID]
	if mapstatus != nil {
		for _, infom := range mapstatus.PInfo {
			if info, ok := netstatus.PInfo[infom.IfName]; ok {
				log.Debugf("copyProbeStats: (%s) on %s, info/map success %d/%d, fail %d/%d\n",
					netstatus.BridgeName, info.IfName, info.SuccessCnt, infom.SuccessCnt, info.FailedCnt, infom.FailedCnt)
				info.SuccessCnt = infom.SuccessCnt
				info.FailedCnt = infom.FailedCnt
				info.SuccessProbeCnt = infom.SuccessProbeCnt
				info.FailedProbeCnt = infom.FailedProbeCnt
				info.TransDown = infom.TransDown
				netstatus.PInfo[info.IfName] = info
			}
		}
	}
}
