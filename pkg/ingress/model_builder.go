package ingress

import (
	"context"
	awssdk "github.com/aws/aws-sdk-go/aws"
	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	networking "k8s.io/api/networking/v1beta1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/aws-load-balancer-controller/pkg/annotations"
	"sigs.k8s.io/aws-load-balancer-controller/pkg/aws/services"
	"sigs.k8s.io/aws-load-balancer-controller/pkg/k8s"
	"sigs.k8s.io/aws-load-balancer-controller/pkg/model/core"
	ec2model "sigs.k8s.io/aws-load-balancer-controller/pkg/model/ec2"
	elbv2model "sigs.k8s.io/aws-load-balancer-controller/pkg/model/elbv2"
	networkingpkg "sigs.k8s.io/aws-load-balancer-controller/pkg/networking"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	eventWarningConflictSettings = "ConflictSettings"
)

// ModelBuilder is responsible for build mode stack for a IngressGroup.
type ModelBuilder interface {
	// build mode stack for a IngressGroup.
	Build(ctx context.Context, ingGroup Group) (core.Stack, *elbv2model.LoadBalancer, error)
}

// NewDefaultModelBuilder constructs new defaultModelBuilder.
func NewDefaultModelBuilder(k8sClient client.Client, eventRecorder record.EventRecorder,
	ec2Client services.EC2, acmClient services.ACM,
	annotationParser annotations.Parser, subnetsResolver networkingpkg.SubnetsResolver,
	authConfigBuilder AuthConfigBuilder, enhancedBackendBuilder EnhancedBackendBuilder,
	vpcID string, clusterName string, logger logr.Logger) *defaultModelBuilder {
	certDiscovery := NewACMCertDiscovery(acmClient, logger)
	return &defaultModelBuilder{
		k8sClient:              k8sClient,
		eventRecorder:          eventRecorder,
		ec2Client:              ec2Client,
		vpcID:                  vpcID,
		clusterName:            clusterName,
		annotationParser:       annotationParser,
		subnetsResolver:        subnetsResolver,
		certDiscovery:          certDiscovery,
		authConfigBuilder:      authConfigBuilder,
		enhancedBackendBuilder: enhancedBackendBuilder,
	}
}

var _ ModelBuilder = &defaultModelBuilder{}

// default implementation for ModelBuilder
type defaultModelBuilder struct {
	k8sClient     client.Client
	eventRecorder record.EventRecorder
	ec2Client     services.EC2

	vpcID       string
	clusterName string

	annotationParser       annotations.Parser
	subnetsResolver        networkingpkg.SubnetsResolver
	certDiscovery          CertDiscovery
	authConfigBuilder      AuthConfigBuilder
	enhancedBackendBuilder EnhancedBackendBuilder
}

// build mode stack for a IngressGroup.
func (b *defaultModelBuilder) Build(ctx context.Context, ingGroup Group) (core.Stack, *elbv2model.LoadBalancer, error) {
	stack := core.NewDefaultStack(core.StackID(ingGroup.ID))
	task := &defaultModelBuildTask{
		k8sClient:              b.k8sClient,
		eventRecorder:          b.eventRecorder,
		ec2Client:              b.ec2Client,
		vpcID:                  b.vpcID,
		clusterName:            b.clusterName,
		annotationParser:       b.annotationParser,
		subnetsResolver:        b.subnetsResolver,
		certDiscovery:          b.certDiscovery,
		authConfigBuilder:      b.authConfigBuilder,
		enhancedBackendBuilder: b.enhancedBackendBuilder,

		ingGroup: ingGroup,
		stack:    stack,

		defaultIPAddressType:                      elbv2model.IPAddressTypeIPV4,
		defaultScheme:                             elbv2model.LoadBalancerSchemeInternal,
		defaultSSLPolicy:                          "ELBSecurityPolicy-2016-08",
		defaultTargetType:                         elbv2model.TargetTypeInstance,
		defaultBackendProtocol:                    elbv2model.ProtocolHTTP,
		defaultHealthCheckPath:                    "/",
		defaultHealthCheckIntervalSeconds:         15,
		defaultHealthCheckTimeoutSeconds:          5,
		defaultHealthCheckHealthyThresholdCount:   2,
		defaultHealthCheckUnhealthyThresholdCount: 2,
		defaultHealthCheckMatcherHTTPCode:         "200",

		loadBalancer: nil,
		tgByResID:    make(map[string]*elbv2model.TargetGroup),
	}
	if err := task.run(ctx); err != nil {
		return nil, nil, err
	}
	return task.stack, task.loadBalancer, nil
}

// the default model build task
type defaultModelBuildTask struct {
	k8sClient              client.Client
	eventRecorder          record.EventRecorder
	ec2Client              services.EC2
	vpcID                  string
	clusterName            string
	annotationParser       annotations.Parser
	subnetsResolver        networkingpkg.SubnetsResolver
	certDiscovery          CertDiscovery
	authConfigBuilder      AuthConfigBuilder
	enhancedBackendBuilder EnhancedBackendBuilder

	ingGroup Group
	stack    core.Stack

	defaultIPAddressType                      elbv2model.IPAddressType
	defaultScheme                             elbv2model.LoadBalancerScheme
	defaultSSLPolicy                          string
	defaultTargetType                         elbv2model.TargetType
	defaultBackendProtocol                    elbv2model.Protocol
	defaultHealthCheckPath                    string
	defaultHealthCheckTimeoutSeconds          int64
	defaultHealthCheckIntervalSeconds         int64
	defaultHealthCheckHealthyThresholdCount   int64
	defaultHealthCheckUnhealthyThresholdCount int64
	defaultHealthCheckMatcherHTTPCode         string

	loadBalancer *elbv2model.LoadBalancer
	managedSG    *ec2model.SecurityGroup
	tgByResID    map[string]*elbv2model.TargetGroup
}

func (t *defaultModelBuildTask) run(ctx context.Context) error {
	if len(t.ingGroup.Members) == 0 {
		return nil
	}

	ingListByPort := make(map[int64][]*networking.Ingress)
	listenPortConfigsByPort := make(map[int64]map[types.NamespacedName]listenPortConfig)
	for _, ing := range t.ingGroup.Members {
		listenPortConfigByPortForIngress, err := t.computeIngressListenPortConfigByPort(ctx, ing)
		if err != nil {
			return err
		}
		ingKey := k8s.NamespacedName(ing)
		for port, cfg := range listenPortConfigByPortForIngress {
			ingListByPort[port] = append(ingListByPort[port], ing)
			if _, exists := listenPortConfigsByPort[port]; !exists {
				listenPortConfigsByPort[port] = make(map[types.NamespacedName]listenPortConfig)
			}
			listenPortConfigsByPort[port][ingKey] = cfg
		}
	}
	listenPortConfigByPort := make(map[int64]listenPortConfig)
	for port, cfgs := range listenPortConfigsByPort {
		mergedCfg, err := t.mergeListenPortConfigs(ctx, cfgs)
		if err != nil {
			return errors.Wrapf(err, "failed to merge listPort config for port: %v", port)
		}
		listenPortConfigByPort[port] = mergedCfg
	}

	lb, err := t.buildLoadBalancer(ctx, listenPortConfigByPort)
	if err != nil {
		return err
	}
	for port, cfg := range listenPortConfigByPort {
		ingList := ingListByPort[port]
		ls, err := t.buildListener(ctx, lb.LoadBalancerARN(), port, cfg, ingList)
		if err != nil {
			return err
		}
		if err := t.buildListenerRules(ctx, ls.ListenerARN(), port, cfg.protocol, ingList); err != nil {
			return err
		}
	}

	if err := t.buildLoadBalancerAddOns(ctx, lb.LoadBalancerARN()); err != nil {
		return err
	}
	return nil
}

func (t *defaultModelBuildTask) mergeListenPortConfigs(_ context.Context, listenPortConfigByIngress map[types.NamespacedName]listenPortConfig) (listenPortConfig, error) {
	var mergedProtocol *elbv2model.Protocol
	var mergedProtocolProvider types.NamespacedName
	var mergedInboundCIDRv4s []string
	var mergedInboundCIDRv6s []string
	var mergedInboundCIDRsProvider types.NamespacedName
	var mergedSSLPolicy *string
	var mergedSSLPolicyProvider types.NamespacedName
	mergedTLSCerts := sets.NewString()

	for ingKey, cfg := range listenPortConfigByIngress {
		if mergedProtocol == nil {
			protocol := cfg.protocol
			mergedProtocol = &protocol
			mergedProtocolProvider = ingKey
		} else if (*mergedProtocol) != cfg.protocol {
			return listenPortConfig{}, errors.Errorf("conflicting protocol, %v: %v | %v: %v",
				mergedProtocolProvider, *mergedProtocol, ingKey, cfg.protocol)
		}

		definedCIDRsInCfg := len(cfg.inboundCIDRv4s) != 0 || len(cfg.inboundCIDRv6s) != 0
		if definedCIDRsInCfg {
			definedCIDRsInMergedCfg := len(mergedInboundCIDRv4s) != 0 || len(mergedInboundCIDRv6s) != 0
			if !definedCIDRsInMergedCfg {
				mergedInboundCIDRv4s = cfg.inboundCIDRv4s
				mergedInboundCIDRv6s = cfg.inboundCIDRv6s
			} else {
				return listenPortConfig{}, errors.Errorf("conflicting sslPolicy, %v: %v, %v | %v: %v, %v",
					mergedInboundCIDRsProvider, mergedInboundCIDRv4s, mergedInboundCIDRv6s, ingKey, cfg.inboundCIDRv4s, cfg.inboundCIDRv6s)
			}
		}

		if cfg.sslPolicy != nil {
			if mergedSSLPolicy == nil {
				mergedSSLPolicy = cfg.sslPolicy
				mergedSSLPolicyProvider = ingKey
			} else if awssdk.StringValue(mergedSSLPolicy) != awssdk.StringValue(cfg.sslPolicy) {
				return listenPortConfig{}, errors.Errorf("conflicting sslPolicy, %v: %v | %v: %v",
					mergedSSLPolicyProvider, awssdk.StringValue(mergedSSLPolicy), ingKey, awssdk.StringValue(cfg.sslPolicy))
			}
		}
		mergedTLSCerts.Insert(cfg.tlsCerts...)
	}

	if mergedProtocol == nil {
		return listenPortConfig{}, errors.New("should never happen")
	}

	if len(mergedInboundCIDRv4s) == 0 && len(mergedInboundCIDRv6s) == 0 {
		mergedInboundCIDRv4s = append(mergedInboundCIDRv4s, "0.0.0.0/0")
		mergedInboundCIDRv6s = append(mergedInboundCIDRv6s, "::/0")
	}
	if *mergedProtocol == elbv2model.ProtocolHTTPS && mergedSSLPolicy == nil {
		mergedSSLPolicy = awssdk.String(t.defaultSSLPolicy)
	}

	return listenPortConfig{
		protocol:       *mergedProtocol,
		inboundCIDRv4s: mergedInboundCIDRv4s,
		inboundCIDRv6s: mergedInboundCIDRv6s,
		sslPolicy:      mergedSSLPolicy,
		tlsCerts:       mergedTLSCerts.List(),
	}, nil
}