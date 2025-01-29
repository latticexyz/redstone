package altda

import (
	"testing"

	"github.com/ethereum-optimism/optimism/op-node/rollup/derive/params"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/require"
)

func encodeCommitmentData(commitmentType CommitmentType, data []byte) []byte {
	return append([]byte{byte(commitmentType)}, data...)
}

// TestCommitmentData tests the CommitmentData type and its implementations,
// by encoding and decoding the commitment data and verifying the input data.
func TestCommitmentData(t *testing.T) {
	t.Parallel()

	type tcase struct {
		name        string
		commType    CommitmentType
		commInput   []byte
		commData    []byte
		expectedErr error
	}

	input := []byte{0}
	hash := crypto.Keccak256(input)

	testCases := []tcase{
		{
			name:        "valid keccak256 commitment",
			commType:    Keccak256CommitmentType,
			commInput:   input,
			commData:    encodeCommitmentData(Keccak256CommitmentType, hash),
			expectedErr: nil,
		},
		{
			name:        "invalid keccak256 commitment",
			commType:    Keccak256CommitmentType,
			commInput:   input,
			commData:    encodeCommitmentData(Keccak256CommitmentType, []byte("ab_baddata_yz012345")),
			expectedErr: ErrInvalidCommitment,
		},
		{
			name:        "valid generic commitment",
			commType:    GenericCommitmentType,
			commInput:   []byte("any input works"),
			commData:    encodeCommitmentData(GenericCommitmentType, []byte("any length of data! wow, that's so generic!")),
			expectedErr: nil, // This should actually be valid now
		},
		{
			name:        "invalid commitment type",
			commType:    9,
			commInput:   []byte("some input"),
			commData:    encodeCommitmentData(CommitmentType(9), []byte("abcdefghijklmnopqrstuvwxyz012345")),
			expectedErr: ErrInvalidCommitment,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			comm, err := DecodeCommitmentData(tc.commData)
			require.ErrorIs(t, err, tc.expectedErr)
			if err == nil {
				// Test that the commitment type is correct
				require.Equal(t, tc.commType, comm.CommitmentType())
				// Test that reencoding the commitment returns the same data
				require.Equal(t, tc.commData, comm.Encode())
				// Test that TxData() returns the same data as the original, prepended with a version byte
				require.Equal(t, append([]byte{params.DerivationVersion1}, tc.commData...), comm.TxData())

				// Test that Verify() returns no error for the correct data
				require.NoError(t, comm.Verify(tc.commInput))
				// Test that Verify() returns error for the incorrect data
				// don't do this for GenericCommitmentType, which does not do any verification
				if tc.commType != GenericCommitmentType {
					require.ErrorIs(t, ErrCommitmentMismatch, comm.Verify([]byte("wrong data")))
				}
			}
		})
	}
}


func TestBatchedCommitment(t *testing.T) {
	t.Parallel()

	t.Run("empty batch", func(t *testing.T) {
		_, err := NewBatchedCommitment(nil)
		require.ErrorIs(t, err, ErrInvalidCommitment)
	})

	t.Run("mixed types", func(t *testing.T) {
		comms := []CommitmentData{
			NewKeccak256Commitment([]byte("data1")),
			NewGenericCommitment([]byte("data2")),
		}
		_, err := NewBatchedCommitment(comms)
		require.Error(t, err)
	})

	t.Run("valid keccak batch", func(t *testing.T) {
		inputs := [][]byte{
			[]byte("data1"),
			[]byte("data2"),
			[]byte("data3"),
		}
		comms := make([]CommitmentData, len(inputs))
		for i, input := range inputs {
			comms[i] = NewKeccak256Commitment(input)
		}

		// Create batch
		batch, err := NewBatchedCommitment(comms)
		require.NoError(t, err)

		// Decode batch
		decoded, err := batch.GetCommitments()
		require.NoError(t, err)
		require.Equal(t, len(comms), len(decoded))

		// Verify each commitment matches and can verify its input
		for i, comm := range decoded {
			require.Equal(t, Keccak256CommitmentType, comm.CommitmentType())
			require.NoError(t, comm.Verify(inputs[i]))
			require.Equal(t, comms[i].Encode(), comm.Encode())
		}
	})

	t.Run("valid generic batch", func(t *testing.T) {
		datas := [][]byte{
			[]byte("generic1"),
			[]byte("generic2"),
			[]byte("generic3"),
		}
		comms := make([]CommitmentData, len(datas))
		for i, data := range datas {
			comms[i] = NewGenericCommitment(data)
		}

		// Create batch
		batch, err := NewBatchedCommitment(comms)
		require.NoError(t, err)

		// Test batch encoding/decoding
		decoded, err := batch.GetCommitments()
		require.NoError(t, err)
		require.Equal(t, len(comms), len(decoded))

		// Verify each commitment matches
		for i, comm := range decoded {
			require.Equal(t, GenericCommitmentType, comm.CommitmentType())
			require.Equal(t, comms[i].Encode(), comm.Encode())
		}
	})

	t.Run("malformed batch data", func(t *testing.T) {
		testCases := []struct {
			name string
			data []byte
		}{
			{"empty data", []byte{}},
			{"only type byte", []byte{byte(Keccak256CommitmentType)}},
			{"incomplete length", []byte{byte(Keccak256CommitmentType), 0}},
			{"length with no data", []byte{byte(Keccak256CommitmentType), 0, 32}},
			{"invalid type", []byte{255, 0, 32, 1, 2, 3}},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				_, err := DecodeBatchedCommitment(tc.data)
				require.ErrorIs(t, err, ErrInvalidCommitment)
			})
		}
	})

	t.Run("batch roundtrip", func(t *testing.T) {
		// Create a batch
		comms := []CommitmentData{
			NewKeccak256Commitment([]byte("data1")),
			NewKeccak256Commitment([]byte("data2")),
		}
		batch, err := NewBatchedCommitment(comms)
		require.NoError(t, err)

		// Encode it
		encoded := batch.Encode()

		// Decode it
		decoded, err := DecodeCommitmentData(encoded)
		require.NoError(t, err)

		// Verify it's a batched commitment
		batchComm, ok := decoded.(BatchedCommitment)
		require.True(t, ok)

		// Get the individual commitments
		decodedComms, err := batchComm.GetCommitments()
		require.NoError(t, err)

		// Verify they match the original
		require.Equal(t, len(comms), len(decodedComms))
		for i := range comms {
			require.Equal(t, comms[i].Encode(), decodedComms[i].Encode())
		}
	})
}
