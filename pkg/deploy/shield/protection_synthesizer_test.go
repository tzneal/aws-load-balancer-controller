package shield_test

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/golang/mock/gomock"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/aws-load-balancer-controller/pkg/deploy/shield"
	"sigs.k8s.io/aws-load-balancer-controller/pkg/model/core"
	shieldmodel "sigs.k8s.io/aws-load-balancer-controller/pkg/model/shield"
)

func TestProtectionSynthesizerHandlesNoResources(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	stack := core.NewMockStack(ctrl)
	pmgr := shield.NewMockProtectionManager(ctrl)

	ps := shield.NewProtectionSynthesizer(pmgr, logr.New(&log.NullLogSink{}), stack)
	stack.EXPECT().ListResources(gomock.Any()).Return(nil)
	if err := ps.Synthesize(context.Background()); err != nil {
		t.Fatalf("expected no error, got %s", err)
	}
}

func TestProtectionSynthesizerHandlesCreateProtection(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	stack := core.NewMockStack(ctrl)
	pmgr := shield.NewMockProtectionManager(ctrl)

	ps := shield.NewProtectionSynthesizer(pmgr, logr.New(&log.NullLogSink{}), stack)

	stack.EXPECT().AddResource(gomock.Any())
	protection := shieldmodel.NewProtection(stack, "foo", shieldmodel.ProtectionSpec{
		Enabled:     true,
		ResourceARN: core.LiteralStringToken("arn"),
	})
	resources := []*shieldmodel.Protection{protection}
	stack.EXPECT().ListResources(gomock.Any()).SetArg(0, resources).Return(nil)

	pmgr.EXPECT().GetProtection(gomock.Any(), "arn").Return(nil, nil)

	// should crate the protection
	pmgr.EXPECT().CreateProtection(gomock.Any(), "arn", "managed by aws-load-balancer-controller").Return("", nil)
	if err := ps.Synthesize(context.Background()); err != nil {
		t.Fatalf("expected no error, got %s", err)
	}
}

func TestProtectionSynthesizerHandlesRemovesProtection(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	stack := core.NewMockStack(ctrl)
	pmgr := shield.NewMockProtectionManager(ctrl)

	ps := shield.NewProtectionSynthesizer(pmgr, logr.New(&log.NullLogSink{}), stack)

	stack.EXPECT().AddResource(gomock.Any())
	protection := shieldmodel.NewProtection(stack, "foo", shieldmodel.ProtectionSpec{
		Enabled:     false,
		ResourceARN: core.LiteralStringToken("arn"),
	})
	resources := []*shieldmodel.Protection{protection}
	stack.EXPECT().ListResources(gomock.Any()).SetArg(0, resources).Return(nil)

	protectionInfo := &shield.ProtectionInfo{
		Name: "managed by aws-load-balancer-controller",
		ID:   "id",
	}
	pmgr.EXPECT().GetProtection(gomock.Any(), "arn").Return(protectionInfo, nil)

	// should delete the protection
	pmgr.EXPECT().DeleteProtection(gomock.Any(), "arn", "id").Return(nil)
	if err := ps.Synthesize(context.Background()); err != nil {
		t.Fatalf("expected no error, got %s", err)
	}
}

func TestProtectionSynthesizerIgnoresUnknownProtection(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	stack := core.NewMockStack(ctrl)
	pmgr := shield.NewMockProtectionManager(ctrl)

	ps := shield.NewProtectionSynthesizer(pmgr, logr.New(&log.NullLogSink{}), stack)

	stack.EXPECT().AddResource(gomock.Any())
	protection := shieldmodel.NewProtection(stack, "foo", shieldmodel.ProtectionSpec{
		Enabled:     false,
		ResourceARN: core.LiteralStringToken("arn"),
	})
	resources := []*shieldmodel.Protection{protection}
	stack.EXPECT().ListResources(gomock.Any()).SetArg(0, resources).Return(nil)

	protectionInfo := &shield.ProtectionInfo{
		Name: "managed by someone-else",
		ID:   "id",
	}
	pmgr.EXPECT().GetProtection(gomock.Any(), "arn").Return(protectionInfo, nil)

	//  no delete call here since the name of the protection info is not the ALB

	if err := ps.Synthesize(context.Background()); err != nil {
		t.Fatalf("expected no error, got %s", err)
	}
}
