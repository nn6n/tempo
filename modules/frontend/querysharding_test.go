package frontend

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-kit/kit/log"
	"github.com/gogo/protobuf/proto"
	"github.com/grafana/tempo/pkg/model"
	"github.com/grafana/tempo/pkg/tempopb"
	"github.com/grafana/tempo/pkg/util/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/weaveworks/common/user"
)

func TestCreateBlockBoundaries(t *testing.T) {
	tests := []struct {
		name        string
		queryShards int
		expected    [][]byte
	}{
		{
			name:        "single shard",
			queryShards: 1,
			expected: [][]byte{
				{0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0},
				{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
			},
		},
		{
			name:        "multiple shards",
			queryShards: 4,
			expected: [][]byte{
				{0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0},  // 0
				{0x3f, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0}, // 0x3f = 255/4 * 1
				{0x7e, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0}, // 0x7e = 255/4 * 2
				{0xbd, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0}, // 0xbd = 255/4 * 3
				{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bb := createBlockBoundaries(tt.queryShards)
			assert.Len(t, bb, len(tt.expected))

			for i := 0; i < len(bb); i++ {
				assert.Equal(t, tt.expected[i], bb[i])
			}
		})
	}
}

func TestBuildShardedRequests(t *testing.T) {
	queryShards := 2

	sharder := &shardQuery{
		queryShards:     queryShards,
		blockBoundaries: createBlockBoundaries(queryShards - 1),
	}

	ctx := user.InjectOrgID(context.Background(), "blerg")
	req := httptest.NewRequest("GET", "/", nil).WithContext(ctx)

	shardedReqs, err := sharder.buildShardedRequests(req)
	require.NoError(t, err)
	require.Len(t, shardedReqs, queryShards)

	require.Equal(t, "/querier/?mode=ingesters", shardedReqs[0].RequestURI)
	require.Equal(t, "/querier/?blockEnd=ffffffffffffffffffffffffffffffff&blockStart=00000000000000000000000000000000&mode=blocks", shardedReqs[1].RequestURI)
}

func TestShardingWareDoRequest(t *testing.T) {
	// create and split a trace
	trace := test.MakeTrace(10, []byte{0x01, 0x02})
	trace1 := &tempopb.Trace{}
	trace2 := &tempopb.Trace{}

	for _, b := range trace.Batches {
		if rand.Int()%2 == 0 {
			trace1.Batches = append(trace1.Batches, b)
		} else {
			trace2.Batches = append(trace2.Batches, b)
		}
	}

	tests := []struct {
		name               string
		status1            int
		status2            int
		trace1             *tempopb.Trace
		trace2             *tempopb.Trace
		err1               error
		err2               error
		failedBlockQueries int
		expectedStatus     int
		expectedTrace      *tempopb.Trace
		expectedError      error
	}{
		{
			name:           "empty returns",
			status1:        200,
			status2:        200,
			expectedStatus: 200,
			expectedTrace:  &tempopb.Trace{},
		},
		{
			name:           "404",
			status1:        404,
			status2:        404,
			expectedStatus: 404,
		},
		{
			name:           "400",
			status1:        400,
			status2:        400,
			expectedStatus: 500,
		},
		{
			name:           "500+404",
			status1:        500,
			status2:        404,
			expectedStatus: 500,
		},
		{
			name:           "404+500",
			status1:        404,
			status2:        500,
			expectedStatus: 500,
		},
		{
			name:           "500+200",
			status1:        500,
			status2:        200,
			trace2:         trace2,
			expectedStatus: 500,
		},
		{
			name:           "200+500",
			status1:        200,
			trace1:         trace1,
			status2:        500,
			expectedStatus: 500,
		},
		{
			name:           "503+200",
			status1:        503,
			status2:        200,
			trace2:         trace2,
			expectedStatus: 500,
		},
		{
			name:           "200+503",
			status1:        200,
			trace1:         trace1,
			status2:        503,
			expectedStatus: 500,
		},
		{
			name:           "200+404",
			status1:        200,
			trace1:         trace1,
			status2:        404,
			expectedStatus: 200,
			expectedTrace:  trace1,
		},
		{
			name:           "404+200",
			status1:        404,
			status2:        200,
			trace2:         trace1,
			expectedStatus: 200,
			expectedTrace:  trace1,
		},
		{
			name:           "200+200",
			status1:        200,
			trace1:         trace1,
			status2:        200,
			trace2:         trace2,
			expectedStatus: 200,
			expectedTrace:  trace,
		},
		{
			name:          "200+err",
			status1:       200,
			trace1:        trace1,
			err2:          errors.New("booo"),
			expectedError: errors.New("booo"),
		},
		{
			name:          "err+200",
			err1:          errors.New("booo"),
			status2:       200,
			trace2:        trace1,
			expectedError: errors.New("booo"),
		},

		{
			name:          "500+err",
			status1:       500,
			trace1:        trace1,
			err2:          errors.New("booo"),
			expectedError: errors.New("booo"),
		},
		{
			name:               "failed block queries <= max",
			status1:            200,
			trace1:             trace1,
			status2:            200,
			trace2:             trace2,
			failedBlockQueries: 1,
			expectedStatus:     200,
			expectedTrace:      trace,
		},
		{
			name:               "too many failed block queries",
			status1:            200,
			trace1:             trace1,
			status2:            200,
			trace2:             trace2,
			failedBlockQueries: 10,
			expectedError:      errors.New("too many failed block queries 10 (max 2)"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sharder := ShardingWare(2, 2, log.NewNopLogger())

			next := RoundTripperFunc(func(r *http.Request) (*http.Response, error) {
				var trace *tempopb.Trace
				var statusCode int
				var err error
				if r.RequestURI == "/querier/api/traces/1234?mode=ingesters" {
					trace = tc.trace1
					statusCode = tc.status1
					err = tc.err1
				} else {
					trace = tc.trace2
					err = tc.err2
					statusCode = tc.status2
				}

				if err != nil {
					return nil, err
				}

				var resBytes []byte
				if trace != nil {
					resBytes, err = proto.Marshal(&tempopb.TraceByIDResponse{
						Trace: trace,
						Metrics: &tempopb.TraceByIDMetrics{
							FailedBlocks: uint32(tc.failedBlockQueries),
						},
					})
					require.NoError(t, err)
				}

				return &http.Response{
					Body:       io.NopCloser(bytes.NewReader(resBytes)),
					StatusCode: statusCode,
				}, nil
			})

			testRT := NewRoundTripper(next, sharder)

			req := httptest.NewRequest("GET", "/api/traces/1234", nil)
			ctx := req.Context()
			ctx = user.InjectOrgID(ctx, "blerg")
			req = req.WithContext(ctx)

			resp, err := testRT.RoundTrip(req)
			if tc.expectedError != nil {
				assert.Equal(t, tc.expectedError, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.expectedStatus, resp.StatusCode)
			if tc.expectedTrace != nil {
				actualResp := &tempopb.TraceByIDResponse{}
				bytesTrace, err := io.ReadAll(resp.Body)
				require.NoError(t, err)
				err = proto.Unmarshal(bytesTrace, actualResp)
				require.NoError(t, err)

				model.SortTrace(tc.expectedTrace)
				model.SortTrace(actualResp.Trace)
				assert.True(t, proto.Equal(tc.expectedTrace, actualResp.Trace))
			}
		})
	}
}
