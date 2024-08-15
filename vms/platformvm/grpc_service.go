// Copyright (C) 2019-2024, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package platformvm

import (
	"context"

	"github.com/ava-labs/avalanchego/proto/pb/platform"
)

var _ platform.PlatformServer = (*grpcService)(nil)

type grpcService struct {
	vm *VM
	platform.UnimplementedPlatformServer
}

func (g *grpcService) GetHeight(ctx context.Context, _ *platform.GetHeightRequest) (*platform.GetHeightReply, error) {
	height, err := g.vm.GetCurrentHeight(ctx)
	if err != nil {
		return nil, err
	}

	return &platform.GetHeightReply{
		Height: height,
	}, nil
}
