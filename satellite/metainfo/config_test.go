// Copyright (C) 2020 Storj Labs, Inc.
// See LICENSE for copying information.

package metainfo_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"storj.io/common/memory"
	"storj.io/common/testrand"
	"storj.io/storj/satellite/metainfo"
)

func TestRSConfigValidation(t *testing.T) {
	tests := []struct {
		description    string
		configString   string
		expectedConfig metainfo.RSConfig
		expectError    bool
	}{
		{
			description:  "valid rs config",
			configString: "4/8/10/20-256B",
			expectedConfig: metainfo.RSConfig{
				ErasureShareSize: 256 * memory.B, Min: 4, Repair: 8, Success: 10, Total: 20,
			},
			expectError: false,
		},
		{
			description:  "invalid rs config - numbers decrease",
			configString: "4/8/5/20-256B",
			expectError:  true,
		},
		{
			description:  "invalid rs config - starts at 0",
			configString: "0/2/4/6-256B",
			expectError:  true,
		},
		{
			description:  "invalid rs config - strings",
			configString: "4/a/b/20-256B",
			expectError:  true,
		},
		{
			description:  "invalid rs config - floating-point numbers",
			configString: "4/5.2/7/20-256B",
			expectError:  true,
		},
		{
			description:  "invalid rs config - not enough items",
			configString: "4/5/20-256B",
			expectError:  true,
		},
		{
			description:  "invalid rs config - too many items",
			configString: "4/5/20/30/50-256B",
			expectError:  true,
		},
		{
			description:  "invalid rs config - empty numbers",
			configString: "-256B",
			expectError:  true,
		},
		{
			description:  "invalid rs config - empty size",
			configString: "1/2/3/4-",
			expectError:  true,
		},
		{
			description:  "invalid rs config - empty",
			configString: "",
			expectError:  true,
		},
		{
			description:  "invalid valid rs config - invalid share size",
			configString: "4/8/10/20-256A",
			expectError:  true,
		},
	}

	for _, tt := range tests {
		t.Log(tt.description)

		rsConfig := metainfo.RSConfig{}
		err := rsConfig.Set(tt.configString)
		if tt.expectError {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
			require.EqualValues(t, tt.expectedConfig.ErasureShareSize, rsConfig.ErasureShareSize)
			require.EqualValues(t, tt.expectedConfig.Min, rsConfig.Min)
			require.EqualValues(t, tt.expectedConfig.Repair, rsConfig.Repair)
			require.EqualValues(t, tt.expectedConfig.Success, rsConfig.Success)
			require.EqualValues(t, tt.expectedConfig.Total, rsConfig.Total)
		}
	}
}

func TestUUIDsFlag(t *testing.T) {
	var UUIDs metainfo.UUIDsFlag
	err := UUIDs.Set("")
	require.NoError(t, err)
	require.Len(t, UUIDs, 0)

	testIDA := testrand.UUID()
	err = UUIDs.Set(testIDA.String())
	require.NoError(t, err)
	require.Equal(t, metainfo.UUIDsFlag{
		testIDA: {},
	}, UUIDs)

	testIDB := testrand.UUID()
	err = UUIDs.Set(testIDA.String() + "," + testIDB.String())
	require.NoError(t, err)
	require.Equal(t, metainfo.UUIDsFlag{
		testIDA: {},
		testIDB: {},
	}, UUIDs)
}
