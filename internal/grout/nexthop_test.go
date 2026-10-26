// SPDX-License-Identifier:Apache-2.0

package grout

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEnsureNeighborAgeing(t *testing.T) {
	const configShow = "grcli --err-exit --json --socket sock nexthop config show"

	tests := []struct {
		name    string
		calls   []cmdCall
		wantErr bool
	}{
		{
			name: "sets the lifetime when it differs",
			calls: []cmdCall{
				{cmd: configShow, output: `{"used":52,"max":4096,"lifetime":1200,"unreachable":60}`},
				{cmd: "grcli --err-exit --json --socket sock nexthop config set lifetime 30"},
			},
		},
		{
			name: "leaves the lifetime alone when already set",
			calls: []cmdCall{
				{cmd: configShow, output: `{"used":52,"max":4096,"lifetime":30,"unreachable":60}`},
			},
		},
		{
			name:    "fails on unparsable config",
			calls:   []cmdCall{{cmd: configShow, output: "not json"}},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			defer mockCmdExec(tc.calls...)()

			err := EnsureNeighborAgeing(context.Background(), NewClient("sock"))
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
		})
	}
}
