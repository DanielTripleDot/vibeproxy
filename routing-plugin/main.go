// The C ABI follows CLIProxyAPI's examples/plugin/scheduler (MIT licensed).
package main

/*
#include <stdint.h>
#include <stdlib.h>
typedef struct { void* ptr; size_t len; } cliproxy_buffer;
typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);
typedef struct { uint32_t abi_version; void* host_ctx; cliproxy_host_call_fn call; cliproxy_host_free_fn free_buffer; } cliproxy_host_api;
typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);
typedef struct {
    uint32_t abi_version;
    cliproxy_plugin_call_fn call;
    cliproxy_plugin_free_fn free_buffer;
    cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;
extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
static int call_host(cliproxy_host_api* host, char* method, uint8_t* request, size_t len, cliproxy_buffer* response) {
    return host->call(host->host_ctx, method, request, len, response);
}
static void free_host_response(cliproxy_host_api* host, cliproxy_buffer* response) {
    if (response->ptr) host->free_buffer(response->ptr, response->len);
}
*/
import "C"

import (
	"encoding/json"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

var scheduler = newScheduler(fetchQuota)

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if host == nil || plugin == nil || host.call == nil || host.free_buffer == nil || uint32(host.abi_version) != pluginabi.ABIVersion {
		return 1
	}
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	scheduler = newScheduler(fetchQuota)
	native := &nativeHost{api: *host}
	scheduler.load = native.credential
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response == nil {
		return 1
	}
	response.ptr, response.len = nil, 0
	if method == nil || requestLen > 16<<20 || (request == nil && requestLen != 0) {
		return 1
	}
	var raw []byte
	if request != nil && requestLen > 0 {
		raw = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	result, err := handleMethod(C.GoString(method), raw)
	var envelope any
	if err != nil {
		// Never expose request bodies, credentials, or upstream error bodies.
		envelope = map[string]any{"ok": false, "error": map[string]string{"code": "invalid_request", "message": "Invalid quota scheduler request"}}
	} else {
		envelope = map[string]any{"ok": true, "result": result}
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return 1
	}
	response.ptr = C.CBytes(encoded)
	response.len = C.size_t(len(encoded))
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() { scheduler.shutdown() }

type nativeHost struct{ api C.cliproxy_host_api }

func (h *nativeHost) call(method string, request any, result any) error {
	raw, err := json.Marshal(request)
	if err != nil {
		return errQuotaUnavailable
	}
	cMethod, cRequest := C.CString(method), C.CBytes(raw)
	defer C.free(unsafe.Pointer(cMethod))
	defer C.free(cRequest)
	var response C.cliproxy_buffer
	code := C.call_host(&h.api, cMethod, (*C.uint8_t)(cRequest), C.size_t(len(raw)), &response)
	defer C.free_host_response(&h.api, &response)
	if code != 0 || response.ptr == nil || response.len == 0 || response.len > 16<<20 {
		return errQuotaUnavailable
	}
	var envelope pluginabi.Envelope
	if err := json.Unmarshal(C.GoBytes(response.ptr, C.int(response.len)), &envelope); err != nil || !envelope.OK {
		return errQuotaUnavailable
	}
	if err := json.Unmarshal(envelope.Result, result); err != nil {
		return errQuotaUnavailable
	}
	return nil
}

func (h *nativeHost) credential(candidate pluginapi.SchedulerAuthCandidate, proxy string) (quotaCredential, error) {
	var roster struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if err := h.call(pluginabi.MethodHostAuthList, struct{}{}, &roster); err != nil {
		return quotaCredential{}, err
	}
	for _, account := range roster.Files {
		if account.ID != candidate.ID {
			continue
		}
		provider := account.Provider
		if provider == "" {
			provider = account.Type
		}
		if provider != candidate.Provider || account.Disabled || account.RuntimeOnly || account.BaseURL != "" || account.AuthIndex == "" {
			break
		}
		var result pluginapi.HostAuthGetResponse
		if err := h.call(pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: account.AuthIndex}, &result); err != nil {
			return quotaCredential{}, err
		}
		if result.AuthIndex != account.AuthIndex {
			break
		}
		var metadata map[string]any
		if err := json.Unmarshal(result.JSON, &metadata); err != nil {
			break
		}
		candidate.Metadata = metadata
		cred := credentialFor(candidate, proxy)
		if cred.token == "" {
			break
		}
		return cred, nil
	}
	return quotaCredential{}, errQuotaUnavailable
}

func handleMethod(method string, raw []byte) (any, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var lifecycle struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if err := json.Unmarshal(raw, &lifecycle); err != nil {
			return nil, err
		}
		var cfg struct {
			ProxyURL string `yaml:"proxy-url"`
		}
		if err := yaml.Unmarshal(lifecycle.ConfigYAML, &cfg); err != nil {
			return nil, err
		}
		scheduler.configure(cfg.ProxyURL)
		return map[string]any{
			"schema_version": pluginabi.SchemaVersion,
			"metadata": pluginapi.Metadata{
				Name: "VibeProxy quota-aware routing", Version: "1.0.0", Author: "VibeProxy",
				GitHubRepository: "https://github.com/automazeio/vibeproxy",
			},
			"capabilities": map[string]bool{"scheduler": true, "scheduler_across_priorities": true},
		}, nil
	case pluginabi.MethodSchedulerPick:
		var req pluginapi.SchedulerPickRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		return scheduler.pick(req), nil
	default:
		return nil, &json.UnsupportedValueError{}
	}
}
