/**
 * Tencent is pleased to support the open source community by making Polaris available.
 *
 * Copyright (C) 2019 THL A29 Limited, a Tencent company. All rights reserved.
 *
 * Licensed under the BSD 3-Clause License (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * https://opensource.org/licenses/BSD-3-Clause
 *
 * Unless required by applicable law or agreed to in writing, software distributed
 * under the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR
 * CONDITIONS OF ANY KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations under the License.
 */

package xdsserverv3

import (
	"fmt"

	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	httpinspector "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/listener/http_inspector/v3"
	original_dstv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/listener/original_dst/v3"
	tlsinspector "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/listener/tls_inspector/v3"
	hcm "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	tlstrans "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/envoyproxy/go-control-plane/pkg/wellknown"
	"github.com/golang/protobuf/ptypes/wrappers"

	"github.com/polarismesh/polaris/apiserver/xdsserverv3/resource"
	"github.com/polarismesh/polaris/common/model"
	"github.com/polarismesh/polaris/service"
)

var (
	tlsFilters = []*listenerv3.ListenerFilter{
		{
			Name: "envoy.filters.listener.http_inspector",
			ConfigType: &listenerv3.ListenerFilter_TypedConfig{
				TypedConfig: resource.MustNewAny(&httpinspector.HttpInspector{}),
			},
		},
		{
			Name: "envoy.filters.listener.tls_inspector",
			ConfigType: &listenerv3.ListenerFilter_TypedConfig{
				TypedConfig: resource.MustNewAny(&tlsinspector.TlsInspector{}),
			},
		},
	}

	defaultListenerFilters = []*listenerv3.ListenerFilter{
		{
			// type.googleapis.com/envoy.extensions.filters.listener.original_dst.v3.OriginalDst
			Name: wellknown.OriginalDestination,
			ConfigType: &listenerv3.ListenerFilter_TypedConfig{
				TypedConfig: resource.MustNewAny(&original_dstv3.OriginalDst{}),
			},
		},
	}

	boundBindPort = map[corev3.TrafficDirection]uint32{
		// envoy -> sidecar 方向 envoy 的监听端口，主要是 EnvoyGateway 以及 Sidecar InBound 场景
		core.TrafficDirection_INBOUND: 15006,
		// sidecar -> envoy 方向 envoy 的监听端口
		core.TrafficDirection_OUTBOUND: 15001,
	}
)

// LDSBuilder .
type LDSBuilder struct {
	client *resource.XDSClient
	svr    service.DiscoverServer
}

func (lds *LDSBuilder) Init(clien *resource.XDSClient, svr service.DiscoverServer) {
	lds.client = clien
	lds.svr = svr
}

func (lds *LDSBuilder) Generate(option *resource.BuildOption) (interface{}, error) {
	var resources []types.Resource

	switch lds.client.RunType {
	case resource.RunTypeGateway:
		ret, err := lds.makeListener(option, core.TrafficDirection_OUTBOUND)
		if err != nil {
			return nil, err
		}
		resources = ret
	case resource.RunTypeSidecar:
		inBoundListener, err := lds.makeListener(option, corev3.TrafficDirection_INBOUND)
		if err != nil {
			return nil, err
		}
		outBoundListener, err := lds.makeListener(option, corev3.TrafficDirection_OUTBOUND)
		if err != nil {
			return nil, err
		}
		resources = append(resources, inBoundListener...)
		resources = append(resources, outBoundListener...)
	}
	return resources, nil
}

func (lds *LDSBuilder) makeListener(option *resource.BuildOption,
	direction corev3.TrafficDirection) ([]types.Resource, error) {

	var boundHCM *hcm.HttpConnectionManager
	if lds.client.IsGateway() {
		boundHCM = resource.MakeGatewayBoundHCM()
	} else {
		selfService := model.ServiceKey{
			Namespace: lds.client.GetSelfNamespace(),
			Name:      lds.client.GetSelfService(),
		}
		boundHCM = resource.MakeSidecarBoundHCM(selfService, direction)
	}

	listener := makeDefaultListener(direction, boundHCM)
	listener.ListenerFilters = append(listener.ListenerFilters, defaultListenerFilters...)

	if option.TLSMode != resource.TLSModeNone {
		listener.FilterChains = []*listenerv3.FilterChain{
			{
				FilterChainMatch: &listenerv3.FilterChainMatch{
					TransportProtocol: "tls",
				},
				TransportSocket: resource.MakeTLSTransportSocket(&tlstrans.DownstreamTlsContext{
					CommonTlsContext: resource.InboundCommonTLSContext,
					RequireClientCertificate: &wrappers.BoolValue{
						Value: true,
					},
				}),
				Filters: []*listenerv3.Filter{
					{
						Name: "envoy.filters.network.http_connection_manager",
						ConfigType: &listenerv3.Filter_TypedConfig{
							TypedConfig: resource.MustNewAny(boundHCM),
						},
					}},
				Name: "PassthroughFilterChain-TLS",
			},
		}

		listener.ListenerFilters = append(tlsFilters, listener.ListenerFilters...)
		if option.TLSMode == resource.TLSModeStrict {
			listener.DefaultFilterChain = nil
		}
	}

	return []types.Resource{
		listener,
	}, nil
}

func makeDefaultListener(trafficDirection corev3.TrafficDirection,
	boundHCM *hcm.HttpConnectionManager) *listenerv3.Listener {

	bindPort := boundBindPort[trafficDirection]
	trafficDirectionName := corev3.TrafficDirection_name[int32(trafficDirection)]
	listener := &listenerv3.Listener{
		Name:             fmt.Sprintf("%s_%d", trafficDirectionName, bindPort),
		TrafficDirection: trafficDirection,
		Address: &core.Address{
			Address: &core.Address_SocketAddress{
				SocketAddress: &core.SocketAddress{
					Protocol: core.SocketAddress_TCP,
					Address:  "0.0.0.0",
					PortSpecifier: &core.SocketAddress_PortValue{
						PortValue: bindPort,
					},
				},
			},
		},
		FilterChains: []*listenerv3.FilterChain{
			{
				Filters: []*listenerv3.Filter{
					{
						Name: wellknown.HTTPConnectionManager,
						ConfigType: &listenerv3.Filter_TypedConfig{
							TypedConfig: resource.MustNewAny(boundHCM),
						},
					},
				},
			},
		},
		DefaultFilterChain: resource.MakeDefaultFilterChain(),
		ListenerFilters:    []*listenerv3.ListenerFilter{},
	}
	return listener
}
