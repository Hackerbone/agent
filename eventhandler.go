package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
)

type EventHandler struct {
	CorrelationId        string
	Repo                 string
	ApiClient            *ApiClient
	DNSProxy             *DNSProxy
	ProcessConnectionMap map[string]bool
	ProcessFileMap       map[string]bool
	ProcessMap           map[string]*Process
	SourceCodeMap        map[string][]*Event
	netMutex             sync.RWMutex
	fileMutex            sync.RWMutex
	procMutex            sync.RWMutex
}

var classAPrivateSubnet, classBPrivateSubnet, classCPrivateSubnet, loopBackSubnet, ipv6LinkLocalSubnet, ipv6LocalSubnet *net.IPNet

func (eventHandler *EventHandler) handleFileEvent(event *Event) {
	eventHandler.fileMutex.Lock()

	if !strings.HasPrefix(event.FileName, "/") {
		event.FileName = path.Join(event.Path, event.FileName)
	}

	if strings.Contains(event.FileName, "post_event.json") {
		WriteLog("post_event called")

		// send done signal to post step
		writeDone()
	}

	_, found := eventHandler.ProcessFileMap[event.Pid]
	fileType := ""
	if !found {
		// TODO: Improve this logic to monitor dependencies across languages
		if strings.Contains(event.FileName, "/node_modules/") && strings.HasSuffix(event.FileName, ".js") {
			fileType = "Dependencies"

		} else if strings.Contains(event.FileName, ".git/objects") {
			fileType = "Source Code"
		}

		if fileType != "" {
			tool := *eventHandler.GetToolChain(event.PPid, event.Exe)
			eventHandler.ApiClient.sendFileEvent(eventHandler.CorrelationId, eventHandler.Repo, fileType, event.Timestamp, tool)
			eventHandler.ProcessFileMap[event.Pid] = true
		}
	}

	if isSourceCodeFile(event.FileName) && !isSyscallExcluded(event.Syscall) {
		_, found = eventHandler.SourceCodeMap[event.FileName]
		if !found {
			eventHandler.SourceCodeMap[event.FileName] = append(eventHandler.SourceCodeMap[event.FileName], event)
		}
		if found {
			isFromDifferentProcess := false
			for _, writeEvent := range eventHandler.SourceCodeMap[event.FileName] {
				if writeEvent.Pid != event.Pid {
					isFromDifferentProcess = true
				}
			}

			if isFromDifferentProcess {
				eventHandler.SourceCodeMap[event.FileName] = append(eventHandler.SourceCodeMap[event.FileName], event)
				WriteAnnotation(fmt.Sprintf("Source code overwritten %s syscall: %s by %s", event.FileName, event.Syscall, event.Exe))
			}
		}
	}

	eventHandler.fileMutex.Unlock()
}

func isSyscallExcluded(syscall string) bool {
	if syscall == "chmod" || syscall == "unlink" || syscall == "unlinkat" {
		return true
	}

	return false
}

func isSourceCodeFile(fileName string) bool {
	ext := path.Ext(fileName)
	// https://docs.github.com/en/get-started/learning-about-github/github-language-support
	sourceCodeExtensions := []string{".c", "cpp", "cs", ".go", ".java", ".js", ".php", "py", ".rb", ".rs", ".scala", ".sc", ".sh", ".ts"}
	for _, extension := range sourceCodeExtensions {
		if ext == extension {
			return true
		}
	}

	return false
}

func (eventHandler *EventHandler) handleProcessEvent(event *Event) {
	eventHandler.procMutex.Lock()

	_, found := eventHandler.ProcessMap[event.Pid]

	if !found {
		eventHandler.ProcessMap[event.Pid] = &Process{PID: event.Pid, PPid: event.PPid, Exe: event.Exe, Arguments: event.ProcessArguments}
	}

	eventHandler.procMutex.Unlock()
}

func (eventHandler *EventHandler) handleNetworkEvent(event *Event) {
	eventHandler.netMutex.Lock()

	if !isPrivateIPAddress(event.IPAddress) &&
		strings.Compare(event.IPAddress, "::1") != 0 &&
		strings.Compare(event.IPAddress, AzureIPAddress) != 0 &&
		strings.Compare(event.IPAddress, MetadataIPAddress) != 0 {

		cacheKey := fmt.Sprintf("%s%s%s", event.Pid, event.IPAddress, event.Port)

		_, found := eventHandler.ProcessConnectionMap[cacheKey]

		if !found {
			tool := Tool{}
			image := GetContainerByPid(event.Pid)
			if image == "" {
				if event.Exe != "" {
					tool = *eventHandler.GetToolChain(event.PPid, event.Exe)
				}

			} else {
				tool = Tool{Name: image, SHA256: image} // TODO: Set container image checksum
			}

			reverseLookUp := eventHandler.DNSProxy.GetReverseIPLookup(event.IPAddress)
			eventHandler.ApiClient.sendNetConnection(eventHandler.CorrelationId, eventHandler.Repo, event.IPAddress, event.Port, reverseLookUp, "", event.Timestamp, tool)
			eventHandler.ProcessConnectionMap[cacheKey] = true
		}
	}

	eventHandler.netMutex.Unlock()
}

func (eventHandler *EventHandler) HandleEvent(event *Event) {
	switch event.EventType {
	case netMonitorTag:
		eventHandler.handleNetworkEvent(event)
	case fileMonitorTag:
		eventHandler.handleFileEvent(event)
	case processMonitorTag:
		eventHandler.handleProcessEvent(event)
	}
}

func GetContainerByPid(pid string) string {
	cgroupPath := fmt.Sprintf("/proc/%s/cgroup", pid)
	content, _ := ioutil.ReadFile(cgroupPath)

	ctx := context.Background()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		//panic(err)
	}

	containers, err := cli.ContainerList(ctx, types.ContainerListOptions{})
	if err != nil {
		//panic(err)
	}

	for _, container := range containers {
		json, _ := cli.ContainerInspect(ctx, container.ID)
		if strings.Compare(pid, fmt.Sprintf("%d", json.State.Pid)) == 0 {
			return container.Image
		} else if strings.Contains(string(content), container.ID) {
			return container.Image
		}
	}

	return ""
}

func getProgramChecksum(path string) (string, error) {

	f, err := os.Open(path)
	if err != nil {
		return err.Error(), err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err.Error(), err
	}

	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func (eventHandler *EventHandler) GetToolChain(ppid, exe string) *Tool {
	checksum, _ := getProgramChecksum(exe)
	tool := Tool{Name: filepath.Base(exe), SHA256: checksum}

	// In some cases the process has already exited, so get from map first
	eventHandler.procMutex.Lock()
	parentProcess, found := eventHandler.ProcessMap[ppid]
	eventHandler.procMutex.Unlock()

	if found {
		tool.Parent = eventHandler.GetToolChain(parentProcess.PPid, parentProcess.Exe)
		return &tool
	}

	// If not in map, may be long running, so get from OS
	parentProcessId, err := getParentProcessId(ppid)
	if err != nil {
		return &tool
	}

	path, err := getProcessExe(ppid)
	if err != nil {
		return &tool
	}

	tool.Parent = eventHandler.GetToolChain(fmt.Sprintf("%d", parentProcessId), path)

	return &tool
}

func isPrivateIPAddress(ipAddress string) bool {
	
	if ipAddress == AllZeros {
		return true
	}

	if classAPrivateSubnet == nil {
		_, classAPrivateSubnet, _ = net.ParseCIDR(classAPrivateAddressRange)
	}
	if classBPrivateSubnet == nil {
		_, classBPrivateSubnet, _ = net.ParseCIDR(classBPrivateAddressRange)
	}
	if classCPrivateSubnet == nil {
		_, classCPrivateSubnet, _ = net.ParseCIDR(classCPrivateAddressRange)
	}
	if loopBackSubnet == nil {
		_, loopBackSubnet, _ = net.ParseCIDR(loopBackAddressRange)
	}
	if ipv6LinkLocalSubnet == nil {
		_, ipv6LinkLocalSubnet, _ = net.ParseCIDR(ipv6LinkLocalAddressRange)
	}
	if ipv6LocalSubnet == nil {
		_, ipv6LocalSubnet, _ = net.ParseCIDR(ipv6LocalAddressRange)
	}

	ip := net.ParseIP(ipAddress)

	if classAPrivateSubnet.Contains(ip) {
		return true
	}

	if classBPrivateSubnet.Contains(ip) {
		return true
	}

	if classCPrivateSubnet.Contains(ip) {
		return true
	}

	if loopBackSubnet.Contains(ip) {
		return true
	}

	if ipv6LinkLocalSubnet.Contains(ip) {
		return true
	}

	if ipv6LocalSubnet.Contains(ip) {
		return true
	}

	// https://gist.github.com/nanmu42/9c8139e15542b3c4a1709cb9e9ac61eb
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}

	return false
}
