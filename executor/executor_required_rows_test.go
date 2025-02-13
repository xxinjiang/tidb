// Copyright 2019 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package executor

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/pingcap/tidb/domain"
	"github.com/pingcap/tidb/executor/internal/exec"
	"github.com/pingcap/tidb/expression"
	"github.com/pingcap/tidb/expression/aggregation"
	"github.com/pingcap/tidb/parser/ast"
	"github.com/pingcap/tidb/parser/mysql"
	plannercore "github.com/pingcap/tidb/planner/core"
	"github.com/pingcap/tidb/planner/util"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/sessionctx/variable"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/util/chunk"
	"github.com/pingcap/tidb/util/disk"
	"github.com/pingcap/tidb/util/mathutil"
	"github.com/pingcap/tidb/util/memory"
	"github.com/pingcap/tidb/util/mock"
	"github.com/stretchr/testify/require"
)

type requiredRowsDataSource struct {
	exec.BaseExecutor
	totalRows int
	count     int
	ctx       sessionctx.Context

	expectedRowsRet []int
	numNextCalled   int

	generator func(valType *types.FieldType) interface{}
}

func newRequiredRowsDataSourceWithGenerator(ctx sessionctx.Context, totalRows int, expectedRowsRet []int,
	gen func(valType *types.FieldType) interface{}) *requiredRowsDataSource {
	ds := newRequiredRowsDataSource(ctx, totalRows, expectedRowsRet)
	ds.generator = gen
	return ds
}

func newRequiredRowsDataSource(ctx sessionctx.Context, totalRows int, expectedRowsRet []int) *requiredRowsDataSource {
	// the schema of output is fixed now, which is [Double, Long]
	retTypes := []*types.FieldType{types.NewFieldType(mysql.TypeDouble), types.NewFieldType(mysql.TypeLonglong)}
	cols := make([]*expression.Column, len(retTypes))
	for i := range retTypes {
		cols[i] = &expression.Column{Index: i, RetType: retTypes[i]}
	}
	schema := expression.NewSchema(cols...)
	baseExec := exec.NewBaseExecutor(ctx, schema, 0)
	return &requiredRowsDataSource{baseExec, totalRows, 0, ctx, expectedRowsRet, 0, defaultGenerator}
}

func (r *requiredRowsDataSource) Next(ctx context.Context, req *chunk.Chunk) error {
	defer func() {
		if r.expectedRowsRet == nil {
			r.numNextCalled++
			return
		}
		rowsRet := req.NumRows()
		expected := r.expectedRowsRet[r.numNextCalled]
		if rowsRet != expected {
			panic(fmt.Sprintf("unexpected number of rows returned, obtain: %v, expected: %v", rowsRet, expected))
		}
		r.numNextCalled++
	}()

	req.Reset()
	if r.count > r.totalRows {
		return nil
	}
	required := mathutil.Min(req.RequiredRows(), r.totalRows-r.count)
	for i := 0; i < required; i++ {
		req.AppendRow(r.genOneRow())
	}
	r.count += required
	return nil
}

func (r *requiredRowsDataSource) genOneRow() chunk.Row {
	row := chunk.MutRowFromTypes(retTypes(r))
	for i, tp := range retTypes(r) {
		row.SetValue(i, r.generator(tp))
	}
	return row.ToRow()
}

func defaultGenerator(valType *types.FieldType) interface{} {
	switch valType.GetType() {
	case mysql.TypeLong, mysql.TypeLonglong:
		return int64(rand.Int())
	case mysql.TypeDouble:
		return rand.Float64()
	default:
		panic("not implement")
	}
}

func (r *requiredRowsDataSource) checkNumNextCalled() error {
	if r.numNextCalled != len(r.expectedRowsRet) {
		return fmt.Errorf("unexpected number of call on Next, obtain: %v, expected: %v",
			r.numNextCalled, len(r.expectedRowsRet))
	}
	return nil
}

func TestLimitRequiredRows(t *testing.T) {
	maxChunkSize := defaultCtx().GetSessionVars().MaxChunkSize
	testCases := []struct {
		totalRows      int
		limitOffset    int
		limitCount     int
		requiredRows   []int
		expectedRows   []int
		expectedRowsDS []int
	}{
		{
			totalRows:      20,
			limitOffset:    0,
			limitCount:     10,
			requiredRows:   []int{3, 5, 1, 500, 500},
			expectedRows:   []int{3, 5, 1, 1, 0},
			expectedRowsDS: []int{3, 5, 1, 1},
		},
		{
			totalRows:      20,
			limitOffset:    0,
			limitCount:     25,
			requiredRows:   []int{9, 500},
			expectedRows:   []int{9, 11},
			expectedRowsDS: []int{9, 11},
		},
		{
			totalRows:      100,
			limitOffset:    50,
			limitCount:     30,
			requiredRows:   []int{10, 5, 10, 20},
			expectedRows:   []int{10, 5, 10, 5},
			expectedRowsDS: []int{60, 5, 10, 5},
		},
		{
			totalRows:      100,
			limitOffset:    101,
			limitCount:     10,
			requiredRows:   []int{10},
			expectedRows:   []int{0},
			expectedRowsDS: []int{100, 0},
		},
		{
			totalRows:      maxChunkSize + 20,
			limitOffset:    maxChunkSize + 1,
			limitCount:     10,
			requiredRows:   []int{3, 3, 3, 100},
			expectedRows:   []int{3, 3, 3, 1},
			expectedRowsDS: []int{maxChunkSize, 4, 3, 3, 1},
		},
	}

	for _, testCase := range testCases {
		sctx := defaultCtx()
		ctx := context.Background()
		ds := newRequiredRowsDataSource(sctx, testCase.totalRows, testCase.expectedRowsDS)
		exe := buildLimitExec(sctx, ds, testCase.limitOffset, testCase.limitCount)
		require.NoError(t, exe.Open(ctx))
		chk := newFirstChunk(exe)
		for i := range testCase.requiredRows {
			chk.SetRequiredRows(testCase.requiredRows[i], sctx.GetSessionVars().MaxChunkSize)
			require.NoError(t, exe.Next(ctx, chk))
			require.Equal(t, testCase.expectedRows[i], chk.NumRows())
		}
		require.NoError(t, exe.Close())
		require.NoError(t, ds.checkNumNextCalled())
	}
}

func buildLimitExec(ctx sessionctx.Context, src exec.Executor, offset, count int) exec.Executor {
	n := mathutil.Min(count, ctx.GetSessionVars().MaxChunkSize)
	base := exec.NewBaseExecutor(ctx, src.Schema(), 0, src)
	base.SetInitCap(n)
	limitExec := &LimitExec{
		BaseExecutor: base,
		begin:        uint64(offset),
		end:          uint64(offset + count),
	}
	return limitExec
}

func defaultCtx() sessionctx.Context {
	ctx := mock.NewContext()
	ctx.GetSessionVars().InitChunkSize = variable.DefInitChunkSize
	ctx.GetSessionVars().MaxChunkSize = variable.DefMaxChunkSize
	ctx.GetSessionVars().StmtCtx.MemTracker = memory.NewTracker(-1, ctx.GetSessionVars().MemQuotaQuery)
	ctx.GetSessionVars().StmtCtx.DiskTracker = disk.NewTracker(-1, -1)
	ctx.GetSessionVars().SnapshotTS = uint64(1)
	domain.BindDomain(ctx, domain.NewMockDomain())
	return ctx
}

func TestSortRequiredRows(t *testing.T) {
	maxChunkSize := defaultCtx().GetSessionVars().MaxChunkSize
	testCases := []struct {
		totalRows      int
		groupBy        []int
		requiredRows   []int
		expectedRows   []int
		expectedRowsDS []int
	}{
		{
			totalRows:      10,
			groupBy:        []int{0},
			requiredRows:   []int{1, 5, 3, 10},
			expectedRows:   []int{1, 5, 3, 1},
			expectedRowsDS: []int{10, 0},
		},
		{
			totalRows:      10,
			groupBy:        []int{0, 1},
			requiredRows:   []int{1, 5, 3, 10},
			expectedRows:   []int{1, 5, 3, 1},
			expectedRowsDS: []int{10, 0},
		},
		{
			totalRows:      maxChunkSize + 1,
			groupBy:        []int{0},
			requiredRows:   []int{1, 5, 3, 10, maxChunkSize},
			expectedRows:   []int{1, 5, 3, 10, (maxChunkSize + 1) - 1 - 5 - 3 - 10},
			expectedRowsDS: []int{maxChunkSize, 1, 0},
		},
		{
			totalRows:      3*maxChunkSize + 1,
			groupBy:        []int{0},
			requiredRows:   []int{1, 5, 3, 10, maxChunkSize},
			expectedRows:   []int{1, 5, 3, 10, maxChunkSize},
			expectedRowsDS: []int{maxChunkSize, maxChunkSize, maxChunkSize, 1, 0},
		},
	}

	for _, testCase := range testCases {
		sctx := defaultCtx()
		ctx := context.Background()
		ds := newRequiredRowsDataSource(sctx, testCase.totalRows, testCase.expectedRowsDS)
		byItems := make([]*util.ByItems, 0, len(testCase.groupBy))
		for _, groupBy := range testCase.groupBy {
			col := ds.Schema().Columns[groupBy]
			byItems = append(byItems, &util.ByItems{Expr: col})
		}
		exec := buildSortExec(sctx, byItems, ds)
		require.NoError(t, exec.Open(ctx))
		chk := newFirstChunk(exec)
		for i := range testCase.requiredRows {
			chk.SetRequiredRows(testCase.requiredRows[i], maxChunkSize)
			require.NoError(t, exec.Next(ctx, chk))
			require.Equal(t, testCase.expectedRows[i], chk.NumRows())
		}
		require.NoError(t, exec.Close())
		require.NoError(t, ds.checkNumNextCalled())
	}
}

func buildSortExec(sctx sessionctx.Context, byItems []*util.ByItems, src exec.Executor) exec.Executor {
	sortExec := SortExec{
		BaseExecutor: exec.NewBaseExecutor(sctx, src.Schema(), 0, src),
		ByItems:      byItems,
		schema:       src.Schema(),
	}
	return &sortExec
}

func TestTopNRequiredRows(t *testing.T) {
	maxChunkSize := defaultCtx().GetSessionVars().MaxChunkSize
	testCases := []struct {
		totalRows      int
		topNOffset     int
		topNCount      int
		groupBy        []int
		requiredRows   []int
		expectedRows   []int
		expectedRowsDS []int
	}{
		{
			totalRows:      10,
			topNOffset:     0,
			topNCount:      10,
			groupBy:        []int{0},
			requiredRows:   []int{1, 1, 1, 1, 10},
			expectedRows:   []int{1, 1, 1, 1, 6},
			expectedRowsDS: []int{10, 0},
		},
		{
			totalRows:      100,
			topNOffset:     15,
			topNCount:      11,
			groupBy:        []int{0},
			requiredRows:   []int{1, 1, 1, 1, 10},
			expectedRows:   []int{1, 1, 1, 1, 7},
			expectedRowsDS: []int{26, 100 - 26, 0},
		},
		{
			totalRows:      100,
			topNOffset:     95,
			topNCount:      10,
			groupBy:        []int{0},
			requiredRows:   []int{1, 2, 3, 10},
			expectedRows:   []int{1, 2, 2, 0},
			expectedRowsDS: []int{100, 0, 0},
		},
		{
			totalRows:      maxChunkSize + 20,
			topNOffset:     1,
			topNCount:      5,
			groupBy:        []int{0, 1},
			requiredRows:   []int{1, 3, 7, 10},
			expectedRows:   []int{1, 3, 1, 0},
			expectedRowsDS: []int{6, maxChunkSize, 14, 0},
		},
		{
			totalRows:      maxChunkSize + maxChunkSize + 20,
			topNOffset:     maxChunkSize + 10,
			topNCount:      8,
			groupBy:        []int{0, 1},
			requiredRows:   []int{1, 2, 3, 5, 7},
			expectedRows:   []int{1, 2, 3, 2, 0},
			expectedRowsDS: []int{maxChunkSize, 18, maxChunkSize, 2, 0},
		},
		{
			totalRows:      maxChunkSize*5 + 10,
			topNOffset:     maxChunkSize*5 + 20,
			topNCount:      10,
			groupBy:        []int{0, 1},
			requiredRows:   []int{1, 2, 3},
			expectedRows:   []int{0, 0, 0},
			expectedRowsDS: []int{maxChunkSize, maxChunkSize, maxChunkSize, maxChunkSize, maxChunkSize, 10, 0, 0},
		},
		{
			totalRows:      maxChunkSize + maxChunkSize + 10,
			topNOffset:     10,
			topNCount:      math.MaxInt64,
			groupBy:        []int{0, 1},
			requiredRows:   []int{1, 2, 3, maxChunkSize, maxChunkSize},
			expectedRows:   []int{1, 2, 3, maxChunkSize, maxChunkSize - 1 - 2 - 3},
			expectedRowsDS: []int{maxChunkSize, maxChunkSize, 10, 0, 0},
		},
	}

	for _, testCase := range testCases {
		sctx := defaultCtx()
		ctx := context.Background()
		ds := newRequiredRowsDataSource(sctx, testCase.totalRows, testCase.expectedRowsDS)
		byItems := make([]*util.ByItems, 0, len(testCase.groupBy))
		for _, groupBy := range testCase.groupBy {
			col := ds.Schema().Columns[groupBy]
			byItems = append(byItems, &util.ByItems{Expr: col})
		}
		exec := buildTopNExec(sctx, testCase.topNOffset, testCase.topNCount, byItems, ds)
		require.NoError(t, exec.Open(ctx))
		chk := newFirstChunk(exec)
		for i := range testCase.requiredRows {
			chk.SetRequiredRows(testCase.requiredRows[i], maxChunkSize)
			require.NoError(t, exec.Next(ctx, chk))
			require.Equal(t, testCase.expectedRows[i], chk.NumRows())
		}
		require.NoError(t, exec.Close())
		require.NoError(t, ds.checkNumNextCalled())
	}
}

func buildTopNExec(ctx sessionctx.Context, offset, count int, byItems []*util.ByItems, src exec.Executor) exec.Executor {
	sortExec := SortExec{
		BaseExecutor: exec.NewBaseExecutor(ctx, src.Schema(), 0, src),
		ByItems:      byItems,
		schema:       src.Schema(),
	}
	return &TopNExec{
		SortExec: sortExec,
		limit:    &plannercore.PhysicalLimit{Count: uint64(count), Offset: uint64(offset)},
	}
}

func TestSelectionRequiredRows(t *testing.T) {
	gen01 := func() func(valType *types.FieldType) interface{} {
		closureCount := 0
		return func(valType *types.FieldType) interface{} {
			switch valType.GetType() {
			case mysql.TypeLong, mysql.TypeLonglong:
				ret := int64(closureCount % 2)
				closureCount++
				return ret
			case mysql.TypeDouble:
				return rand.Float64()
			default:
				panic("not implement")
			}
		}
	}

	maxChunkSize := defaultCtx().GetSessionVars().MaxChunkSize
	testCases := []struct {
		totalRows      int
		filtersOfCol1  int
		requiredRows   []int
		expectedRows   []int
		expectedRowsDS []int
		gen            func(valType *types.FieldType) interface{}
	}{
		{
			totalRows:      20,
			requiredRows:   []int{1, 2, 3, 4, 5, 20},
			expectedRows:   []int{1, 2, 3, 4, 5, 5},
			expectedRowsDS: []int{20, 0},
		},
		{
			totalRows:      20,
			filtersOfCol1:  0,
			requiredRows:   []int{1, 3, 5, 7, 9},
			expectedRows:   []int{1, 3, 5, 1, 0},
			expectedRowsDS: []int{20, 0, 0},
			gen:            gen01(),
		},
		{
			totalRows:      maxChunkSize + 20,
			filtersOfCol1:  1,
			requiredRows:   []int{1, 3, 5, maxChunkSize},
			expectedRows:   []int{1, 3, 5, maxChunkSize/2 - 1 - 3 - 5 + 10},
			expectedRowsDS: []int{maxChunkSize, 20, 0},
			gen:            gen01(),
		},
	}

	for _, testCase := range testCases {
		sctx := defaultCtx()
		ctx := context.Background()
		var filters []expression.Expression
		var ds *requiredRowsDataSource
		if testCase.gen == nil {
			// ignore filters
			ds = newRequiredRowsDataSource(sctx, testCase.totalRows, testCase.expectedRowsDS)
		} else {
			ds = newRequiredRowsDataSourceWithGenerator(sctx, testCase.totalRows, testCase.expectedRowsDS, testCase.gen)
			f, err := expression.NewFunction(
				sctx, ast.EQ, types.NewFieldType(byte(types.ETInt)), ds.Schema().Columns[1], &expression.Constant{
					Value:   types.NewDatum(testCase.filtersOfCol1),
					RetType: types.NewFieldType(mysql.TypeTiny),
				})
			require.NoError(t, err)
			filters = append(filters, f)
		}
		exec := buildSelectionExec(sctx, filters, ds)
		require.NoError(t, exec.Open(ctx))
		chk := newFirstChunk(exec)
		for i := range testCase.requiredRows {
			chk.SetRequiredRows(testCase.requiredRows[i], maxChunkSize)
			require.NoError(t, exec.Next(ctx, chk))
			require.Equal(t, testCase.expectedRows[i], chk.NumRows())
		}
		require.NoError(t, exec.Close())
		require.NoError(t, ds.checkNumNextCalled())
	}
}

func buildSelectionExec(ctx sessionctx.Context, filters []expression.Expression, src exec.Executor) exec.Executor {
	return &SelectionExec{
		BaseExecutor: exec.NewBaseExecutor(ctx, src.Schema(), 0, src),
		filters:      filters,
	}
}

func TestProjectionUnparallelRequiredRows(t *testing.T) {
	maxChunkSize := defaultCtx().GetSessionVars().MaxChunkSize
	testCases := []struct {
		totalRows      int
		requiredRows   []int
		expectedRows   []int
		expectedRowsDS []int
	}{
		{
			totalRows:      20,
			requiredRows:   []int{1, 3, 5, 7, 9},
			expectedRows:   []int{1, 3, 5, 7, 4},
			expectedRowsDS: []int{1, 3, 5, 7, 4},
		},
		{
			totalRows:      maxChunkSize + 10,
			requiredRows:   []int{1, 3, 5, 7, 9, maxChunkSize},
			expectedRows:   []int{1, 3, 5, 7, 9, maxChunkSize - 1 - 3 - 5 - 7 - 9 + 10},
			expectedRowsDS: []int{1, 3, 5, 7, 9, maxChunkSize - 1 - 3 - 5 - 7 - 9 + 10},
		},
		{
			totalRows:      maxChunkSize*2 + 10,
			requiredRows:   []int{1, 7, 9, maxChunkSize, maxChunkSize + 10},
			expectedRows:   []int{1, 7, 9, maxChunkSize, maxChunkSize + 10 - 1 - 7 - 9},
			expectedRowsDS: []int{1, 7, 9, maxChunkSize, maxChunkSize + 10 - 1 - 7 - 9},
		},
	}

	for _, testCase := range testCases {
		sctx := defaultCtx()
		ctx := context.Background()
		ds := newRequiredRowsDataSource(sctx, testCase.totalRows, testCase.expectedRowsDS)
		exprs := make([]expression.Expression, 0, len(ds.Schema().Columns))
		if len(exprs) == 0 {
			for _, col := range ds.Schema().Columns {
				exprs = append(exprs, col)
			}
		}
		exec := buildProjectionExec(sctx, exprs, ds, 0)
		require.NoError(t, exec.Open(ctx))
		chk := newFirstChunk(exec)
		for i := range testCase.requiredRows {
			chk.SetRequiredRows(testCase.requiredRows[i], maxChunkSize)
			require.NoError(t, exec.Next(ctx, chk))
			require.Equal(t, testCase.expectedRows[i], chk.NumRows())
		}
		require.NoError(t, exec.Close())
		require.NoError(t, ds.checkNumNextCalled())
	}
}

func TestProjectionParallelRequiredRows(t *testing.T) {
	t.Skip("not stable because of goroutine schedule")
	maxChunkSize := defaultCtx().GetSessionVars().MaxChunkSize
	testCases := []struct {
		totalRows      int
		numWorkers     int
		requiredRows   []int
		expectedRows   []int
		expectedRowsDS []int
	}{
		{
			totalRows:      20,
			numWorkers:     1,
			requiredRows:   []int{1, 2, 3, 4, 5, 6, 1, 1},
			expectedRows:   []int{1, 1, 2, 3, 4, 5, 4, 0},
			expectedRowsDS: []int{1, 1, 2, 3, 4, 5, 4, 0},
		},
		{
			totalRows:      maxChunkSize * 2,
			numWorkers:     1,
			requiredRows:   []int{7, maxChunkSize, maxChunkSize, maxChunkSize},
			expectedRows:   []int{7, 7, maxChunkSize, maxChunkSize - 14},
			expectedRowsDS: []int{7, 7, maxChunkSize, maxChunkSize - 14, 0},
		},
		{
			totalRows:      20,
			numWorkers:     2,
			requiredRows:   []int{1, 2, 3, 4, 5, 6, 1, 1, 1},
			expectedRows:   []int{1, 1, 1, 2, 3, 4, 5, 3, 0},
			expectedRowsDS: []int{1, 1, 1, 2, 3, 4, 5, 3, 0},
		},
	}

	for _, testCase := range testCases {
		sctx := defaultCtx()
		ctx := context.Background()
		ds := newRequiredRowsDataSource(sctx, testCase.totalRows, testCase.expectedRowsDS)
		exprs := make([]expression.Expression, 0, len(ds.Schema().Columns))
		if len(exprs) == 0 {
			for _, col := range ds.Schema().Columns {
				exprs = append(exprs, col)
			}
		}
		exec := buildProjectionExec(sctx, exprs, ds, testCase.numWorkers)
		require.NoError(t, exec.Open(ctx))
		chk := newFirstChunk(exec)
		for i := range testCase.requiredRows {
			chk.SetRequiredRows(testCase.requiredRows[i], maxChunkSize)
			require.NoError(t, exec.Next(ctx, chk))
			require.Equal(t, testCase.expectedRows[i], chk.NumRows())

			// wait projectionInputFetcher blocked on fetching data
			// from child in the background.
			time.Sleep(time.Millisecond * 25)
		}
		require.NoError(t, exec.Close())
		require.NoError(t, ds.checkNumNextCalled())
	}
}

func buildProjectionExec(ctx sessionctx.Context, exprs []expression.Expression, src exec.Executor, numWorkers int) exec.Executor {
	return &ProjectionExec{
		BaseExecutor:  exec.NewBaseExecutor(ctx, src.Schema(), 0, src),
		numWorkers:    int64(numWorkers),
		evaluatorSuit: expression.NewEvaluatorSuite(exprs, false),
	}
}

func divGenerator(factor int) func(valType *types.FieldType) interface{} {
	closureCountInt := 0
	closureCountDouble := 0
	return func(valType *types.FieldType) interface{} {
		switch valType.GetType() {
		case mysql.TypeLong, mysql.TypeLonglong:
			ret := int64(closureCountInt / factor)
			closureCountInt++
			return ret
		case mysql.TypeDouble:
			ret := float64(closureCountInt / factor)
			closureCountDouble++
			return ret
		default:
			panic("not implement")
		}
	}
}

func TestStreamAggRequiredRows(t *testing.T) {
	maxChunkSize := defaultCtx().GetSessionVars().MaxChunkSize
	testCases := []struct {
		totalRows      int
		aggFunc        string
		requiredRows   []int
		expectedRows   []int
		expectedRowsDS []int
		gen            func(valType *types.FieldType) interface{}
	}{
		{
			totalRows:      1000000,
			aggFunc:        ast.AggFuncSum,
			requiredRows:   []int{1, 2, 3, 4, 5, 6, 7},
			expectedRows:   []int{1, 2, 3, 4, 5, 6, 7},
			expectedRowsDS: []int{maxChunkSize},
			gen:            divGenerator(1),
		},
		{
			totalRows:      maxChunkSize * 3,
			aggFunc:        ast.AggFuncAvg,
			requiredRows:   []int{1, 3},
			expectedRows:   []int{1, 2},
			expectedRowsDS: []int{maxChunkSize, maxChunkSize, maxChunkSize, 0},
			gen:            divGenerator(maxChunkSize),
		},
		{
			totalRows:      maxChunkSize*2 - 1,
			aggFunc:        ast.AggFuncMax,
			requiredRows:   []int{maxChunkSize/2 + 1},
			expectedRows:   []int{maxChunkSize/2 + 1},
			expectedRowsDS: []int{maxChunkSize, maxChunkSize - 1},
			gen:            divGenerator(2),
		},
	}

	for _, testCase := range testCases {
		sctx := defaultCtx()
		ctx := context.Background()
		ds := newRequiredRowsDataSourceWithGenerator(sctx, testCase.totalRows, testCase.expectedRowsDS, testCase.gen)
		childCols := ds.Schema().Columns
		schema := expression.NewSchema(childCols...)
		groupBy := []expression.Expression{childCols[1]}
		aggFunc, err := aggregation.NewAggFuncDesc(sctx, testCase.aggFunc, []expression.Expression{childCols[0]}, true)
		require.NoError(t, err)
		aggFuncs := []*aggregation.AggFuncDesc{aggFunc}
		exec := buildStreamAggExecutor(sctx, ds, schema, aggFuncs, groupBy, 1, true)
		require.NoError(t, exec.Open(ctx))
		chk := newFirstChunk(exec)
		for i := range testCase.requiredRows {
			chk.SetRequiredRows(testCase.requiredRows[i], maxChunkSize)
			require.NoError(t, exec.Next(ctx, chk))
			require.Equal(t, testCase.expectedRows[i], chk.NumRows())
		}
		require.NoError(t, exec.Close())
		require.NoError(t, ds.checkNumNextCalled())
	}
}

func TestMergeJoinRequiredRows(t *testing.T) {
	justReturn1 := func(valType *types.FieldType) interface{} {
		switch valType.GetType() {
		case mysql.TypeLong, mysql.TypeLonglong:
			return int64(1)
		case mysql.TypeDouble:
			return float64(1)
		default:
			panic("not support")
		}
	}
	joinTypes := []plannercore.JoinType{plannercore.RightOuterJoin, plannercore.LeftOuterJoin,
		plannercore.LeftOuterSemiJoin, plannercore.AntiLeftOuterSemiJoin}
	for _, joinType := range joinTypes {
		ctx := defaultCtx()
		required := make([]int, 100)
		for i := range required {
			required[i] = rand.Int()%ctx.GetSessionVars().MaxChunkSize + 1
		}
		innerSrc := newRequiredRowsDataSourceWithGenerator(ctx, 1, nil, justReturn1)             // just return one row: (1, 1)
		outerSrc := newRequiredRowsDataSourceWithGenerator(ctx, 10000000, required, justReturn1) // always return (1, 1)
		exec := buildMergeJoinExec(ctx, joinType, innerSrc, outerSrc)
		require.NoError(t, exec.Open(context.Background()))

		chk := newFirstChunk(exec)
		for i := range required {
			chk.SetRequiredRows(required[i], ctx.GetSessionVars().MaxChunkSize)
			require.NoError(t, exec.Next(context.Background(), chk))
		}
		require.NoError(t, exec.Close())
		require.NoError(t, outerSrc.checkNumNextCalled())
	}
}

func genTestChunk4VecGroupChecker(chkRows []int, sameNum int) (expr []expression.Expression, inputs []*chunk.Chunk) {
	chkNum := len(chkRows)
	numRows := 0
	inputs = make([]*chunk.Chunk, chkNum)
	fts := make([]*types.FieldType, 1)
	fts[0] = types.NewFieldType(mysql.TypeLonglong)
	for i := 0; i < chkNum; i++ {
		inputs[i] = chunk.New(fts, chkRows[i], chkRows[i])
		numRows += chkRows[i]
	}
	var numGroups int
	if numRows%sameNum == 0 {
		numGroups = numRows / sameNum
	} else {
		numGroups = numRows/sameNum + 1
	}

	rand.Seed(time.Now().Unix())
	nullPos := rand.Intn(numGroups)
	cnt := 0
	val := rand.Int63()
	for i := 0; i < chkNum; i++ {
		col := inputs[i].Column(0)
		col.ResizeInt64(chkRows[i], false)
		i64s := col.Int64s()
		for j := 0; j < chkRows[i]; j++ {
			if cnt == sameNum {
				val = rand.Int63()
				cnt = 0
				nullPos--
			}
			if nullPos == 0 {
				col.SetNull(j, true)
			} else {
				i64s[j] = val
			}
			cnt++
		}
	}

	expr = make([]expression.Expression, 1)
	expr[0] = &expression.Column{
		RetType: types.NewFieldTypeBuilder().SetType(mysql.TypeLonglong).SetFlen(mysql.MaxIntWidth).BuildP(),
		Index:   0,
	}
	return
}

func TestVecGroupChecker4GroupCount(t *testing.T) {
	testCases := []struct {
		chunkRows      []int
		expectedGroups int
		expectedFlag   []bool
		sameNum        int
	}{
		{
			chunkRows:      []int{1024, 1},
			expectedGroups: 1025,
			expectedFlag:   []bool{false, false},
			sameNum:        1,
		},
		{
			chunkRows:      []int{1024, 1},
			expectedGroups: 1,
			expectedFlag:   []bool{false, true},
			sameNum:        1025,
		},
		{
			chunkRows:      []int{1, 1},
			expectedGroups: 1,
			expectedFlag:   []bool{false, true},
			sameNum:        2,
		},
		{
			chunkRows:      []int{1, 1},
			expectedGroups: 2,
			expectedFlag:   []bool{false, false},
			sameNum:        1,
		},
		{
			chunkRows:      []int{2, 2},
			expectedGroups: 2,
			expectedFlag:   []bool{false, false},
			sameNum:        2,
		},
		{
			chunkRows:      []int{2, 2},
			expectedGroups: 1,
			expectedFlag:   []bool{false, true},
			sameNum:        4,
		},
	}

	ctx := mock.NewContext()
	for _, testCase := range testCases {
		expr, inputChks := genTestChunk4VecGroupChecker(testCase.chunkRows, testCase.sameNum)
		groupChecker := newVecGroupChecker(ctx, expr)
		groupNum := 0
		for i, inputChk := range inputChks {
			flag, err := groupChecker.splitIntoGroups(inputChk)
			require.NoError(t, err)
			require.Equal(t, testCase.expectedFlag[i], flag)
			if flag {
				groupNum += groupChecker.groupCount - 1
			} else {
				groupNum += groupChecker.groupCount
			}
		}
		require.Equal(t, testCase.expectedGroups, groupNum)
	}
}

func buildMergeJoinExec(ctx sessionctx.Context, joinType plannercore.JoinType, innerSrc, outerSrc exec.Executor) exec.Executor {
	if joinType == plannercore.RightOuterJoin {
		innerSrc, outerSrc = outerSrc, innerSrc
	}

	innerCols := innerSrc.Schema().Columns
	outerCols := outerSrc.Schema().Columns
	j := plannercore.BuildMergeJoinPlan(ctx, joinType, outerCols, innerCols)

	j.SetChildren(&mockPlan{exec: outerSrc}, &mockPlan{exec: innerSrc})
	cols := append(append([]*expression.Column{}, outerCols...), innerCols...)
	schema := expression.NewSchema(cols...)
	j.SetSchema(schema)

	j.CompareFuncs = make([]expression.CompareFunc, 0, len(j.LeftJoinKeys))
	for i := range j.LeftJoinKeys {
		j.CompareFuncs = append(j.CompareFuncs, expression.GetCmpFunction(nil, j.LeftJoinKeys[i], j.RightJoinKeys[i]))
	}

	b := newExecutorBuilder(ctx, nil, nil)
	return b.build(j)
}

type mockPlan struct {
	MockPhysicalPlan
	exec exec.Executor
}

func (mp *mockPlan) GetExecutor() exec.Executor {
	return mp.exec
}

func (mp *mockPlan) Schema() *expression.Schema {
	return mp.exec.Schema()
}

// MemoryUsage of mockPlan is only for testing
func (mp *mockPlan) MemoryUsage() (sum int64) {
	return
}

func TestVecGroupCheckerDATARACE(t *testing.T) {
	ctx := mock.NewContext()

	mTypes := []byte{mysql.TypeVarString, mysql.TypeNewDecimal, mysql.TypeJSON}
	for _, mType := range mTypes {
		exprs := make([]expression.Expression, 1)
		exprs[0] = &expression.Column{
			RetType: types.NewFieldTypeBuilder().SetType(mType).BuildP(),
			Index:   0,
		}
		vgc := newVecGroupChecker(ctx, exprs)

		fts := []*types.FieldType{types.NewFieldType(mType)}
		chk := chunk.New(fts, 1, 1)
		vgc.allocateBuffer = func(evalType types.EvalType, capacity int) (*chunk.Column, error) {
			return chk.Column(0), nil
		}
		vgc.releaseBuffer = func(column *chunk.Column) {}

		switch mType {
		case mysql.TypeVarString:
			chk.Column(0).ReserveString(1)
			chk.Column(0).AppendString("abc")
		case mysql.TypeNewDecimal:
			chk.Column(0).ResizeDecimal(1, false)
			chk.Column(0).Decimals()[0] = *types.NewDecFromInt(123)
		case mysql.TypeJSON:
			chk.Column(0).ReserveJSON(1)
			j := new(types.BinaryJSON)
			require.NoError(t, j.UnmarshalJSON([]byte(fmt.Sprintf(`{"%v":%v}`, 123, 123))))
			chk.Column(0).AppendJSON(*j)
		}

		_, err := vgc.splitIntoGroups(chk)
		require.NoError(t, err)

		switch mType {
		case mysql.TypeVarString:
			require.Equal(t, "abc", vgc.firstRowDatums[0].GetString())
			require.Equal(t, "abc", vgc.lastRowDatums[0].GetString())
			chk.Column(0).ReserveString(1)
			chk.Column(0).AppendString("edf")
			require.Equal(t, "abc", vgc.firstRowDatums[0].GetString())
			require.Equal(t, "abc", vgc.lastRowDatums[0].GetString())
		case mysql.TypeNewDecimal:
			require.Equal(t, "123", vgc.firstRowDatums[0].GetMysqlDecimal().String())
			require.Equal(t, "123", vgc.lastRowDatums[0].GetMysqlDecimal().String())
			chk.Column(0).ResizeDecimal(1, false)
			chk.Column(0).Decimals()[0] = *types.NewDecFromInt(456)
			require.Equal(t, "123", vgc.firstRowDatums[0].GetMysqlDecimal().String())
			require.Equal(t, "123", vgc.lastRowDatums[0].GetMysqlDecimal().String())
		case mysql.TypeJSON:
			require.Equal(t, `{"123": 123}`, vgc.firstRowDatums[0].GetMysqlJSON().String())
			require.Equal(t, `{"123": 123}`, vgc.lastRowDatums[0].GetMysqlJSON().String())
			chk.Column(0).ReserveJSON(1)
			j := new(types.BinaryJSON)
			require.NoError(t, j.UnmarshalJSON([]byte(fmt.Sprintf(`{"%v":%v}`, 456, 456))))
			chk.Column(0).AppendJSON(*j)
			require.Equal(t, `{"123": 123}`, vgc.firstRowDatums[0].GetMysqlJSON().String())
			require.Equal(t, `{"123": 123}`, vgc.lastRowDatums[0].GetMysqlJSON().String())
		}
	}
}
