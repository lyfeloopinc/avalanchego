// Copyright (C) 2019-2024, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package p

import (
	"strings"

	"github.com/onsi/ginkgo/v2"
	"github.com/stretchr/testify/require"

	"github.com/ava-labs/avalanchego/proto/pb/platform"
	"github.com/ava-labs/avalanchego/tests/fixture/e2e"
	"github.com/ava-labs/avalanchego/vms/rpcchainvm/grpcutils"
)

var _ = e2e.DescribePChain("[GRPC Service]", func() {
	ginkgo.It("can get the current height", func() {
		tc := e2e.NewTestContext()
		require := require.New(tc)
		ctx := tc.DefaultContext()
		tc.Outf("connecting to node\n")
		uri := strings.TrimPrefix(e2e.GetEnv(tc).GetRandomNodeURI().URI, "http://")
		conn, err := grpcutils.Dial(uri)
		require.NoError(err)

		tc.Outf("getting current height\n")
		client := platform.NewPlatformClient(conn)
		height, err := client.GetHeight(ctx, &platform.GetHeightRequest{})
		require.NoError(err)
		tc.Outf("got current height: %d\n", height)
	})
})
