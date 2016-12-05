// Copyright 2016 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package plan

import (
	"math"

	"github.com/juju/errors"
	"github.com/ngaut/log"
	"github.com/pingcap/tidb/context"
	"github.com/pingcap/tidb/expression"
	"github.com/pingcap/tidb/infoschema"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/model"
	"github.com/pingcap/tidb/mysql"
	"github.com/pingcap/tidb/plan/statistics"
	"github.com/pingcap/tidb/sessionctx/variable"
	"github.com/pingcap/tidb/terror"
	"github.com/pingcap/tidb/util/types"
)

const (
	netWorkFactor   = 1.5
	memoryFactor    = 5.0
	selectionFactor = 0.8
	distinctFactor  = 0.7
	cpuFactor       = 0.9
	aggFactor       = 0.1
	joinFactor      = 0.3
)

// JoinConcurrency means the number of goroutines that participate in joining.
var JoinConcurrency = 5

func getRowCountByIndexRanges(sc *variable.StatementContext, table *statistics.Table, indexRanges []*IndexRange, indexInfo *model.IndexInfo) (uint64, error) {
	totalCount := float64(0)
	for _, indexRange := range indexRanges {
		count := float64(table.Count)
		i := len(indexRange.LowVal) - 1
		l := indexRange.LowVal[i]
		r := indexRange.HighVal[i]
		var rowCount int64
		var err error
		offset := indexInfo.Columns[i].Offset
		if l.Kind() == types.KindNull && r.Kind() == types.KindMaxValue {
			return uint64(table.Count), nil
		} else if l.Kind() == types.KindMinNotNull {
			rowCount, err = table.Columns[offset].EqualRowCount(sc, types.Datum{})
			if r.Kind() == types.KindMaxValue {
				rowCount = table.Count - rowCount
			} else if err == nil {
				lessCount, err1 := table.Columns[offset].LessRowCount(sc, r)
				rowCount = lessCount - rowCount
				err = err1
			}
		} else if r.Kind() == types.KindMaxValue {
			rowCount, err = table.Columns[offset].GreaterRowCount(sc, l)
		} else {
			compare, err1 := l.CompareDatum(sc, r)
			if err1 != nil {
				return 0, errors.Trace(err1)
			}
			if compare == 0 {
				rowCount, err = table.Columns[offset].EqualRowCount(sc, l)
			} else {
				rowCount, err = table.Columns[offset].BetweenRowCount(sc, l, r)
			}
		}
		if err != nil {
			return 0, errors.Trace(err)
		}
		count = count / float64(table.Count) * float64(rowCount)
		// If the condition is a = 1, b = 1, c = 1, d = 1, we think every a=1, b=1, c=1 only filtrate 1/100 data,
		// so as to avoid collapsing too fast.
		for j := 0; j < i; j++ {
			count = count / float64(100)
		}
		totalCount += count
	}
	// To avoid the totalCount become too small.
	if uint64(totalCount) < 1000 {
		// We will not let the row count less than 1000 to avoid collapsing too fast in the future calculation.
		totalCount = 1000.0
	}
	if totalCount > float64(table.Count) {
		totalCount = float64(table.Count) / 3.0
	}
	return uint64(totalCount), nil
}

func getRowCountByTableRange(sc *variable.StatementContext, statsTbl *statistics.Table, ranges []TableRange, offset int) (uint64, error) {
	var rowCount uint64
	for _, rg := range ranges {
		var cnt int64
		var err error
		if rg.LowVal == math.MinInt64 && rg.HighVal == math.MaxInt64 {
			cnt = statsTbl.Count
		} else if rg.LowVal == math.MinInt64 {
			cnt, err = statsTbl.Columns[offset].LessRowCount(sc, types.NewDatum(rg.HighVal))
		} else if rg.HighVal == math.MaxInt64 {
			cnt, err = statsTbl.Columns[offset].GreaterRowCount(sc, types.NewDatum(rg.LowVal))
		} else {
			if rg.LowVal == rg.HighVal {
				cnt, err = statsTbl.Columns[offset].EqualRowCount(sc, types.NewDatum(rg.LowVal))
			} else {
				cnt, err = statsTbl.Columns[offset].BetweenRowCount(sc, types.NewDatum(rg.LowVal), types.NewDatum(rg.HighVal))
			}
		}
		if err != nil {
			return 0, errors.Trace(err)
		}
		rowCount += uint64(cnt)
	}
	if rowCount > uint64(statsTbl.Count) {
		rowCount = uint64(statsTbl.Count)
	}
	return rowCount, nil
}

func (p *DataSource) convert2TableScan(prop *requiredProperty) (*physicalPlanInfo, error) {
	table := p.Table
	client := p.ctx.GetClient()
	sc := p.ctx.GetSessionVars().StmtCtx
	txn, err := p.ctx.GetTxn(false)
	if err != nil {
		return nil, errors.Trace(err)
	}
	var resultPlan PhysicalPlan
	ts := &PhysicalTableScan{
		Table:               p.Table,
		Columns:             p.Columns,
		TableAsName:         p.TableAsName,
		DBName:              p.DBName,
		physicalTableSource: physicalTableSource{client: client},
	}
	ts.tp = "TableScan"
	ts.allocator = p.allocator
	ts.initIDAndContext(p.ctx)
	if txn != nil {
		ts.readOnly = txn.IsReadOnly()
	} else {
		ts.readOnly = true
	}
	ts.SetSchema(p.GetSchema())
	resultPlan = ts
	if sel, ok := p.GetParentByIndex(0).(*Selection); ok {
		newSel := *sel
		conds := make([]expression.Expression, 0, len(sel.Conditions))
		for _, cond := range sel.Conditions {
			conds = append(conds, cond.Clone())
		}
		ts.AccessCondition, newSel.Conditions = detachTableScanConditions(conds, table)
		if client != nil {
			memDB := infoschema.IsMemoryDB(p.DBName.L)
			if !memDB && client.SupportRequestType(kv.ReqTypeSelect, 0) {
				ts.ConditionPBExpr, ts.conditions, newSel.Conditions = expressionsToPB(sc, newSel.Conditions, client)
			}
		}
		err := buildTableRange(ts)
		if err != nil {
			return nil, errors.Trace(err)
		}
		if len(newSel.Conditions) > 0 {
			newSel.SetChildren(ts)
			newSel.onTable = true
			resultPlan = &newSel
		}
	} else {
		ts.Ranges = []TableRange{{math.MinInt64, math.MaxInt64}}
	}
	statsTbl := p.statisticTable
	rowCount := uint64(statsTbl.Count)
	if table.PKIsHandle {
		for i, colInfo := range ts.Columns {
			if mysql.HasPriKeyFlag(colInfo.Flag) {
				ts.pkCol = p.GetSchema()[i]
				break
			}
		}
		var offset int
		for _, colInfo := range table.Columns {
			if mysql.HasPriKeyFlag(colInfo.Flag) {
				offset = colInfo.Offset
				break
			}
		}
		rowCount, err = getRowCountByTableRange(sc, statsTbl, ts.Ranges, offset)
		if err != nil {
			return nil, errors.Trace(err)
		}
	}
	if ts.ConditionPBExpr != nil {
		rowCount = uint64(float64(rowCount) * selectionFactor)
	}
	return resultPlan.matchProperty(prop, &physicalPlanInfo{count: rowCount}), nil
}

func (p *DataSource) convert2IndexScan(prop *requiredProperty, index *model.IndexInfo) (*physicalPlanInfo, error) {
	statsTbl := p.statisticTable
	var resultPlan PhysicalPlan
	client := p.ctx.GetClient()
	sc := p.ctx.GetSessionVars().StmtCtx
	txn, err := p.ctx.GetTxn(false)
	if err != nil {
		return nil, errors.Trace(err)
	}
	is := &PhysicalIndexScan{
		Index:               index,
		Table:               p.Table,
		Columns:             p.Columns,
		TableAsName:         p.TableAsName,
		OutOfOrder:          true,
		DBName:              p.DBName,
		physicalTableSource: physicalTableSource{client: client},
	}
	is.tp = "IndexScan"
	is.allocator = p.allocator
	is.initIDAndContext(p.ctx)
	if txn != nil {
		is.readOnly = txn.IsReadOnly()
	} else {
		is.readOnly = true
	}
	is.SetSchema(p.schema)
	rowCount := uint64(statsTbl.Count)
	resultPlan = is
	if sel, ok := p.GetParentByIndex(0).(*Selection); ok {
		newSel := *sel
		conds := make([]expression.Expression, 0, len(sel.Conditions))
		for _, cond := range sel.Conditions {
			conds = append(conds, cond.Clone())
		}
		is.AccessCondition, newSel.Conditions = detachIndexScanConditions(conds, is)
		if client != nil {
			memDB := infoschema.IsMemoryDB(p.DBName.L)
			if !memDB && client.SupportRequestType(kv.ReqTypeIndex, 0) {
				is.ConditionPBExpr, is.conditions, newSel.Conditions = expressionsToPB(sc, newSel.Conditions, client)
			}
		}
		err := buildIndexRange(p.ctx.GetSessionVars().StmtCtx, is)
		if err != nil {
			if !terror.ErrorEqual(err, types.ErrTruncated) {
				return nil, errors.Trace(err)
			}
			log.Warn("truncate error in buildIndexRange")
		}
		rowCount, err = getRowCountByIndexRanges(sc, statsTbl, is.Ranges, is.Index)
		if err != nil {
			return nil, errors.Trace(err)
		}
		if len(newSel.Conditions) > 0 {
			newSel.SetChildren(is)
			newSel.onTable = true
			resultPlan = &newSel
		}
	} else {
		rb := rangeBuilder{sc: p.ctx.GetSessionVars().StmtCtx}
		is.Ranges = rb.buildIndexRanges(fullRange, types.NewFieldType(mysql.TypeNull))
	}
	is.DoubleRead = !isCoveringIndex(is.Columns, is.Index.Columns, is.Table.PKIsHandle)
	return resultPlan.matchProperty(prop, &physicalPlanInfo{count: rowCount}), nil
}

func isCoveringIndex(columns []*model.ColumnInfo, indexColumns []*model.IndexColumn, pkIsHandle bool) bool {
	for _, colInfo := range columns {
		if pkIsHandle && mysql.HasPriKeyFlag(colInfo.Flag) {
			continue
		}
		isIndexColumn := false
		for _, indexCol := range indexColumns {
			if colInfo.Name.L == indexCol.Name.L && indexCol.Length == types.UnspecifiedLength {
				isIndexColumn = true
				break
			}
		}
		if !isIndexColumn {
			return false
		}
	}
	return true
}

func (p *DataSource) need2ConsiderIndex(prop *requiredProperty) bool {
	if _, ok := p.parents[0].(*Selection); ok || len(prop.props) > 0 {
		return true
	}
	return false
}

// convert2PhysicalPlan implements the LogicalPlan convert2PhysicalPlan interface.
// If there is no index that matches the required property, the returned physicalPlanInfo
// will be table scan and has the cost of MaxInt64. But this can be ignored because the parent will call
// convert2PhysicalPlan again with an empty *requiredProperty, so the plan with the lowest
// cost will be chosen.
func (p *DataSource) convert2PhysicalPlan(prop *requiredProperty) (*physicalPlanInfo, error) {
	info, err := p.getPlanInfo(prop)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if info != nil {
		return info, nil
	}
	info, err = p.tryToConvert2DummyScan(prop)
	if info != nil || err != nil {
		return info, errors.Trace(err)
	}
	indices, includeTableScan := availableIndices(p.table)
	if includeTableScan {
		info, err = p.convert2TableScan(prop)
		if err != nil {
			return nil, errors.Trace(err)
		}
	}
	if !includeTableScan || p.need2ConsiderIndex(prop) {
		for _, index := range indices {
			indexInfo, err := p.convert2IndexScan(prop, index)
			if err != nil {
				return nil, errors.Trace(err)
			}
			if info == nil || indexInfo.cost < info.cost {
				info = indexInfo
			}
		}
	}
	return info, errors.Trace(p.storePlanInfo(prop, info))
}

// tryToConvert2DummyScan is an optimization which checks if its parent is a selection with a constant condition
// that evaluates to false. If it is, there is no need for a real physical scan, a dummy scan will do.
func (p *DataSource) tryToConvert2DummyScan(prop *requiredProperty) (*physicalPlanInfo, error) {
	sel, isSel := p.GetParentByIndex(0).(*Selection)
	if isSel {
		for _, cond := range sel.Conditions {
			if con, ok := cond.(*expression.Constant); ok {
				result, err := expression.EvalBool(con, nil, p.ctx)
				if err != nil {
					return nil, errors.Trace(err)
				}
				if !result {
					dummy := &PhysicalDummyScan{}
					dummy.tp = "Dummy"
					dummy.allocator = p.allocator
					dummy.initIDAndContext(p.ctx)
					dummy.SetSchema(p.schema)
					info := &physicalPlanInfo{p: dummy}
					p.storePlanInfo(prop, info)
					return info, nil
				}
			}
		}
	}
	return nil, nil
}

// addPlanToResponse creates a *physicalPlanInfo that adds p as the parent of info.
func addPlanToResponse(parent PhysicalPlan, info *physicalPlanInfo) *physicalPlanInfo {
	np := parent.Copy()
	np.SetChildren(info.p)
	return &physicalPlanInfo{p: np, cost: info.cost, count: info.count}
}

// enforceProperty creates a *physicalPlanInfo that satisfies the required property by adding
// sort or limit as the parent of the given physical plan.
func enforceProperty(prop *requiredProperty, info *physicalPlanInfo) *physicalPlanInfo {
	if info.p == nil {
		return info
	}
	if len(prop.props) != 0 {
		items := make([]*ByItems, 0, len(prop.props))
		for _, col := range prop.props {
			items = append(items, &ByItems{Expr: col.col, Desc: col.desc})
		}
		sort := &Sort{
			ByItems:   items,
			ExecLimit: prop.limit,
		}
		sort.SetSchema(info.p.GetSchema())
		sort.correlated = info.p.IsCorrelated()
		info = addPlanToResponse(sort, info)
		count := info.count
		if prop.limit != nil {
			count = prop.limit.Offset + prop.limit.Count
		}
		info.cost += sortCost(count)
	} else if prop.limit != nil {
		limit := prop.limit.Copy().(*Limit)
		limit.SetSchema(info.p.GetSchema())
		limit.correlated = info.p.IsCorrelated()
		info = addPlanToResponse(limit, info)
	}
	if prop.limit != nil && prop.limit.Count < info.count {
		info.count = prop.limit.Count
	}
	return info
}

func sortCost(cnt uint64) float64 {
	if cnt == 0 {
		// If cnt is 0, the log(cnt) will be NAN.
		return 0.0
	}
	return float64(cnt)*math.Log2(float64(cnt))*cpuFactor + memoryFactor*float64(cnt)
}

// removeLimit removes the limit from prop.
func removeLimit(prop *requiredProperty) *requiredProperty {
	ret := &requiredProperty{
		props:      prop.props,
		sortKeyLen: prop.sortKeyLen,
	}
	return ret
}

// convertLimitOffsetToCount changes the limit(offset, count) in prop to limit(0, offset + count).
func convertLimitOffsetToCount(prop *requiredProperty) *requiredProperty {
	ret := &requiredProperty{
		props:      prop.props,
		sortKeyLen: prop.sortKeyLen,
	}
	if prop.limit != nil {
		ret.limit = &Limit{
			Count: prop.limit.Offset + prop.limit.Count,
		}
	}
	return ret
}

func limitProperty(limit *Limit) *requiredProperty {
	return &requiredProperty{limit: limit}
}

// convert2PhysicalPlan implements the LogicalPlan convert2PhysicalPlan interface.
func (p *Limit) convert2PhysicalPlan(prop *requiredProperty) (*physicalPlanInfo, error) {
	info, err := p.getPlanInfo(prop)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if info != nil {
		return info, nil
	}
	info, err = p.GetChildByIndex(0).(LogicalPlan).convert2PhysicalPlan(limitProperty(&Limit{Offset: p.Offset, Count: p.Count}))
	if err != nil {
		return nil, errors.Trace(err)
	}
	info = enforceProperty(prop, info)
	p.storePlanInfo(prop, info)
	return info, nil
}

// convert2PhysicalPlanSemi converts the semi join to *physicalPlanInfo.
func (p *Join) convert2PhysicalPlanSemi(prop *requiredProperty) (*physicalPlanInfo, error) {
	lChild := p.GetChildByIndex(0).(LogicalPlan)
	rChild := p.GetChildByIndex(1).(LogicalPlan)
	allLeft := true
	for _, col := range prop.props {
		if lChild.GetSchema().GetIndex(col.col) == -1 {
			allLeft = false
		}
	}
	join := &PhysicalHashSemiJoin{
		WithAux:         SemiJoinWithAux == p.JoinType,
		EqualConditions: p.EqualConditions,
		LeftConditions:  p.LeftConditions,
		RightConditions: p.RightConditions,
		OtherConditions: p.OtherConditions,
		Anti:            p.anti,
	}
	join.ctx = p.ctx
	join.tp = "HashSemiJoin"
	join.allocator = p.allocator
	join.initIDAndContext(p.ctx)
	join.correlated = p.IsCorrelated()
	join.SetSchema(p.schema)
	lProp := prop
	if !allLeft {
		lProp = &requiredProperty{}
	}
	if p.JoinType == SemiJoin {
		lProp = removeLimit(lProp)
	}
	lInfo, err := lChild.convert2PhysicalPlan(lProp)
	if err != nil {
		return nil, errors.Trace(err)
	}
	rInfo, err := rChild.convert2PhysicalPlan(&requiredProperty{})
	if err != nil {
		return nil, errors.Trace(err)
	}
	if lInfo.p != nil {
		join.correlated = join.correlated || lInfo.p.IsCorrelated()
	}
	if rInfo.p != nil {
		join.correlated = join.correlated || rInfo.p.IsCorrelated()
	}
	resultInfo := join.matchProperty(prop, lInfo, rInfo)
	if p.JoinType == SemiJoin {
		resultInfo.count = uint64(float64(lInfo.count) * selectionFactor)
	} else {
		resultInfo.count = lInfo.count
	}
	if !allLeft {
		resultInfo = enforceProperty(prop, resultInfo)
	} else if p.JoinType == SemiJoin {
		resultInfo = enforceProperty(limitProperty(prop.limit), resultInfo)
	}
	return resultInfo, nil
}

// convert2PhysicalPlanLeft converts the left join to *physicalPlanInfo.
func (p *Join) convert2PhysicalPlanLeft(prop *requiredProperty, innerJoin bool) (*physicalPlanInfo, error) {
	lChild := p.GetChildByIndex(0).(LogicalPlan)
	rChild := p.GetChildByIndex(1).(LogicalPlan)
	allLeft := true
	for _, col := range prop.props {
		if lChild.GetSchema().GetIndex(col.col) == -1 {
			allLeft = false
		}
	}
	join := &PhysicalHashJoin{
		EqualConditions: p.EqualConditions,
		LeftConditions:  p.LeftConditions,
		RightConditions: p.RightConditions,
		OtherConditions: p.OtherConditions,
		SmallTable:      1,
		// TODO: decide concurrency by data size.
		Concurrency:   JoinConcurrency,
		DefaultValues: p.DefaultValues,
	}
	join.tp = "HashLeftJoin"
	join.allocator = p.allocator
	join.initIDAndContext(lChild.context())
	join.SetSchema(p.schema)
	join.correlated = p.IsCorrelated()
	if innerJoin {
		join.JoinType = InnerJoin
	} else {
		join.JoinType = LeftOuterJoin
	}
	lProp := prop
	if !allLeft {
		lProp = &requiredProperty{}
	}
	var lInfo *physicalPlanInfo
	var err error
	if innerJoin {
		lInfo, err = lChild.convert2PhysicalPlan(removeLimit(lProp))
	} else {
		lInfo, err = lChild.convert2PhysicalPlan(convertLimitOffsetToCount(lProp))
	}
	if err != nil {
		return nil, errors.Trace(err)
	}
	rInfo, err := rChild.convert2PhysicalPlan(&requiredProperty{})
	if err != nil {
		return nil, errors.Trace(err)
	}
	if lInfo.p != nil {
		join.correlated = join.correlated || lInfo.p.IsCorrelated()
	}
	if rInfo.p != nil {
		join.correlated = join.correlated || rInfo.p.IsCorrelated()
	}
	resultInfo := join.matchProperty(prop, lInfo, rInfo)
	if !allLeft {
		resultInfo = enforceProperty(prop, resultInfo)
	} else {
		resultInfo = enforceProperty(limitProperty(prop.limit), resultInfo)
	}
	return resultInfo, nil
}

// replaceColsInPropBySchema replaces the columns in original prop with the columns in schema.
func replaceColsInPropBySchema(prop *requiredProperty, schema expression.Schema) *requiredProperty {
	newProps := make([]*columnProp, 0, len(prop.props))
	for _, p := range prop.props {
		idx := schema.GetIndex(p.col)
		if idx == -1 {
			log.Errorf("Can't find column %s in schema", p.col)
		}
		newProps = append(newProps, &columnProp{col: schema[idx], desc: p.desc})
	}
	return &requiredProperty{
		props:      newProps,
		sortKeyLen: prop.sortKeyLen,
		limit:      prop.limit,
	}
}

// convert2PhysicalPlanRight converts the right join to *physicalPlanInfo.
func (p *Join) convert2PhysicalPlanRight(prop *requiredProperty, innerJoin bool) (*physicalPlanInfo, error) {
	lChild := p.GetChildByIndex(0).(LogicalPlan)
	rChild := p.GetChildByIndex(1).(LogicalPlan)
	allRight := true
	for _, col := range prop.props {
		if rChild.GetSchema().GetIndex(col.col) == -1 {
			allRight = false
		}
	}
	join := &PhysicalHashJoin{
		EqualConditions: p.EqualConditions,
		LeftConditions:  p.LeftConditions,
		RightConditions: p.RightConditions,
		OtherConditions: p.OtherConditions,
		// TODO: decide concurrency by data size.
		Concurrency:   JoinConcurrency,
		DefaultValues: p.DefaultValues,
	}
	join.tp = "HashRightJoin"
	join.allocator = p.allocator
	join.initIDAndContext(p.ctx)
	join.correlated = p.IsCorrelated()
	join.SetSchema(p.schema)
	if innerJoin {
		join.JoinType = InnerJoin
	} else {
		join.JoinType = RightOuterJoin
	}
	lInfo, err := lChild.convert2PhysicalPlan(&requiredProperty{})
	if err != nil {
		return nil, errors.Trace(err)
	}
	rProp := prop
	if !allRight {
		rProp = &requiredProperty{}
	} else {
		rProp = replaceColsInPropBySchema(rProp, rChild.GetSchema())
	}
	var rInfo *physicalPlanInfo
	if innerJoin {
		rInfo, err = rChild.convert2PhysicalPlan(removeLimit(rProp))
	} else {
		rInfo, err = rChild.convert2PhysicalPlan(convertLimitOffsetToCount(rProp))
	}
	if err != nil {
		return nil, errors.Trace(err)
	}
	if lInfo.p != nil {
		join.correlated = join.correlated || lInfo.p.IsCorrelated()
	}
	if rInfo.p != nil {
		join.correlated = join.correlated || rInfo.p.IsCorrelated()
	}
	resultInfo := join.matchProperty(prop, lInfo, rInfo)
	if !allRight {
		resultInfo = enforceProperty(prop, resultInfo)
	} else {
		resultInfo = enforceProperty(limitProperty(prop.limit), resultInfo)
	}
	return resultInfo, nil
}

// convert2PhysicalPlan implements the LogicalPlan convert2PhysicalPlan interface.
func (p *Join) convert2PhysicalPlan(prop *requiredProperty) (*physicalPlanInfo, error) {
	info, err := p.getPlanInfo(prop)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if info != nil {
		return info, nil
	}
	switch p.JoinType {
	case SemiJoin, SemiJoinWithAux:
		info, err = p.convert2PhysicalPlanSemi(prop)
		if err != nil {
			return nil, errors.Trace(err)
		}
	case LeftOuterJoin:
		info, err = p.convert2PhysicalPlanLeft(prop, false)
		if err != nil {
			return nil, errors.Trace(err)
		}
	case RightOuterJoin:
		info, err = p.convert2PhysicalPlanRight(prop, false)
		if err != nil {
			return nil, errors.Trace(err)
		}
	default:
		lInfo, err := p.convert2PhysicalPlanLeft(prop, true)
		if err != nil {
			return nil, errors.Trace(err)
		}
		rInfo, err := p.convert2PhysicalPlanRight(prop, true)
		if err != nil {
			return nil, errors.Trace(err)
		}
		if rInfo.cost < lInfo.cost {
			info = rInfo
		} else {
			info = lInfo
		}
	}
	p.storePlanInfo(prop, info)
	return info, nil
}

// convert2PhysicalPlanStream converts the logical aggregation to the stream aggregation *physicalPlanInfo.
func (p *Aggregation) convert2PhysicalPlanStream(prop *requiredProperty) (*physicalPlanInfo, error) {
	for _, aggFunc := range p.AggFuncs {
		if aggFunc.GetMode() == expression.FinalMode {
			return &physicalPlanInfo{cost: math.MaxFloat64}, nil
		}
	}
	agg := &PhysicalAggregation{
		AggType:      StreamedAgg,
		AggFuncs:     p.AggFuncs,
		GroupByItems: p.GroupByItems,
	}
	agg.tp = "StreamAgg"
	agg.allocator = p.allocator
	agg.initIDAndContext(p.ctx)
	agg.correlated = p.IsCorrelated()
	agg.HasGby = len(p.GroupByItems) > 0
	agg.SetSchema(p.schema)
	// TODO: Consider distinct key.
	info := &physicalPlanInfo{cost: math.MaxFloat64}
	gbyCols := p.groupByCols
	if len(gbyCols) != len(p.GroupByItems) {
		// group by a + b is not interested in any order.
		return info, nil
	}
	isSortKey := make([]bool, len(gbyCols))
	newProp := &requiredProperty{
		props: make([]*columnProp, 0, len(gbyCols)),
	}
	for _, pro := range prop.props {
		idx := p.getGbyColIndex(pro.col)
		if idx == -1 {
			return info, nil
		}
		isSortKey[idx] = true
		// We should add columns in aggregation in order to keep index right.
		newProp.props = append(newProp.props, &columnProp{col: gbyCols[idx], desc: pro.desc})
	}
	newProp.sortKeyLen = len(newProp.props)
	for i, col := range gbyCols {
		if !isSortKey[i] {
			newProp.props = append(newProp.props, &columnProp{col: col})
		}
	}
	childInfo, err := p.children[0].(LogicalPlan).convert2PhysicalPlan(newProp)
	if err != nil {
		return nil, errors.Trace(err)
	}
	info = addPlanToResponse(agg, childInfo)
	info.cost += float64(info.count) * cpuFactor
	info.count = uint64(float64(info.count) * aggFactor)
	return info, nil
}

// convert2PhysicalPlanFinalHash converts the logical aggregation to the final hash aggregation *physicalPlanInfo.
func (p *Aggregation) convert2PhysicalPlanFinalHash(x physicalDistSQLPlan, childInfo *physicalPlanInfo) *physicalPlanInfo {
	agg := &PhysicalAggregation{
		AggType:      FinalAgg,
		AggFuncs:     p.AggFuncs,
		GroupByItems: p.GroupByItems,
	}
	agg.tp = "HashAgg"
	agg.allocator = p.allocator
	agg.initIDAndContext(p.ctx)
	agg.correlated = p.IsCorrelated()
	agg.SetSchema(p.schema)
	agg.HasGby = len(p.GroupByItems) > 0
	if childInfo.p != nil {
		agg.correlated = agg.correlated || childInfo.p.IsCorrelated()
	}
	schema := x.addAggregation(p.ctx, agg)
	if len(schema) == 0 {
		return nil
	}
	x.(PhysicalPlan).SetSchema(schema)
	info := addPlanToResponse(agg, childInfo)
	info.count = uint64(float64(info.count) * aggFactor)
	// if we build the final aggregation, it must be the best plan.
	info.cost = 0
	return info
}

// convert2PhysicalPlanCompleteHash converts the logical aggregation to the complete hash aggregation *physicalPlanInfo.
func (p *Aggregation) convert2PhysicalPlanCompleteHash(childInfo *physicalPlanInfo) *physicalPlanInfo {
	agg := &PhysicalAggregation{
		AggType:      CompleteAgg,
		AggFuncs:     p.AggFuncs,
		GroupByItems: p.GroupByItems,
	}
	agg.tp = "HashAgg"
	agg.allocator = p.allocator
	agg.initIDAndContext(p.ctx)
	agg.correlated = p.IsCorrelated()
	agg.HasGby = len(p.GroupByItems) > 0
	agg.SetSchema(p.schema)
	if childInfo.p != nil {
		agg.correlated = agg.correlated || childInfo.p.IsCorrelated()
	}
	info := addPlanToResponse(agg, childInfo)
	info.cost += float64(info.count) * memoryFactor
	info.count = uint64(float64(info.count) * aggFactor)
	return info
}

// convert2PhysicalPlanHash converts the logical aggregation to the physical hash aggregation.
func (p *Aggregation) convert2PhysicalPlanHash() (*physicalPlanInfo, error) {
	childInfo, err := p.children[0].(LogicalPlan).convert2PhysicalPlan(&requiredProperty{})
	if err != nil {
		return nil, errors.Trace(err)
	}
	distinct := false
	for _, fun := range p.AggFuncs {
		if fun.IsDistinct() {
			distinct = true
			break
		}
	}
	if !distinct {
		if x, ok := childInfo.p.(physicalDistSQLPlan); ok {
			info := p.convert2PhysicalPlanFinalHash(x, childInfo)
			if info != nil {
				return info, nil
			}
		}
	}
	return p.convert2PhysicalPlanCompleteHash(childInfo), nil
}

// convert2PhysicalPlan implements the LogicalPlan convert2PhysicalPlan interface.
func (p *Aggregation) convert2PhysicalPlan(prop *requiredProperty) (*physicalPlanInfo, error) {
	planInfo, err := p.getPlanInfo(prop)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if planInfo != nil {
		return planInfo, nil
	}
	limit := prop.limit
	if len(prop.props) == 0 {
		planInfo, err = p.convert2PhysicalPlanHash()
		if err != nil {
			return nil, errors.Trace(err)
		}
	}
	streamInfo, err := p.convert2PhysicalPlanStream(removeLimit(prop))
	if planInfo == nil || streamInfo.cost < planInfo.cost {
		planInfo = streamInfo
	}
	planInfo = enforceProperty(limitProperty(limit), planInfo)
	err = p.storePlanInfo(prop, planInfo)
	return planInfo, errors.Trace(err)
}

// convert2PhysicalPlan implements the LogicalPlan convert2PhysicalPlan interface.
func (p *Union) convert2PhysicalPlan(prop *requiredProperty) (*physicalPlanInfo, error) {
	info, err := p.getPlanInfo(prop)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if info != nil {
		return info, nil
	}
	limit := prop.limit
	childInfos := make([]*physicalPlanInfo, 0, len(p.children))
	var count uint64
	for _, child := range p.GetChildren() {
		newProp := convertLimitOffsetToCount(prop)
		newProp.props = make([]*columnProp, 0, len(prop.props))
		for _, c := range prop.props {
			idx := p.GetSchema().GetIndex(c.col)
			newProp.props = append(newProp.props, &columnProp{col: child.GetSchema()[idx], desc: c.desc})
		}
		info, err = child.(LogicalPlan).convert2PhysicalPlan(newProp)
		count += info.count
		if err != nil {
			return nil, errors.Trace(err)
		}
		childInfos = append(childInfos, info)
	}
	info = p.matchProperty(prop, childInfos...)
	info = enforceProperty(limitProperty(limit), info)
	info.count = count
	p.storePlanInfo(prop, info)
	return info, nil
}

// convert2PhysicalPlan implements the LogicalPlan convert2PhysicalPlan interface.
func (p *Selection) convert2PhysicalPlan(prop *requiredProperty) (*physicalPlanInfo, error) {
	info, err := p.getPlanInfo(prop)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if info != nil {
		return info, nil
	}
	// Firstly, we try to push order.
	info, err = p.convert2PhysicalPlanPushOrder(prop)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if len(prop.props) > 0 {
		// Secondly, we push nothing and enforce this property.
		infoEnforce, err := p.convert2PhysicalPlanEnforce(prop)
		if err != nil {
			return nil, errors.Trace(err)
		}
		if infoEnforce.cost < info.cost {
			info = infoEnforce
		}
	}
	if _, ok := p.GetChildByIndex(0).(*DataSource); !ok {
		info = p.matchProperty(prop, info)
	}
	p.storePlanInfo(prop, info)
	return info, nil
}

func (p *Selection) convert2PhysicalPlanPushOrder(prop *requiredProperty) (*physicalPlanInfo, error) {
	child := p.GetChildByIndex(0).(LogicalPlan)
	limit := prop.limit
	info, err := child.convert2PhysicalPlan(removeLimit(prop))
	if err != nil {
		return nil, errors.Trace(err)
	}
	if limit != nil && info.p != nil {
		if np, ok := info.p.(physicalDistSQLPlan); ok {
			np.addLimit(limit)
			info.count = limit.Count
			info.cost = np.calculateCost(info.count)
		} else {
			info = enforceProperty(&requiredProperty{limit: limit}, info)
		}
	}
	return info, nil
}

// convert2PhysicalPlanEnforce converts a selection to *physicalPlanInfo which does not push the
// required property to the children, but enforce the property instead.
func (p *Selection) convert2PhysicalPlanEnforce(prop *requiredProperty) (*physicalPlanInfo, error) {
	child := p.GetChildByIndex(0).(LogicalPlan)
	info, err := child.convert2PhysicalPlan(&requiredProperty{})
	if err != nil {
		return nil, errors.Trace(err)
	}
	if prop.limit != nil && len(prop.props) > 0 {
		if t, ok := info.p.(physicalDistSQLPlan); ok {
			t.addTopN(p.ctx, prop)
		}
		info = enforceProperty(prop, info)
	} else if len(prop.props) != 0 {
		info.cost = math.MaxFloat64
	}
	return info, nil
}

// convert2PhysicalPlan implements the LogicalPlan convert2PhysicalPlan interface.
func (p *Projection) convert2PhysicalPlan(prop *requiredProperty) (*physicalPlanInfo, error) {
	info, err := p.getPlanInfo(prop)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if info != nil {
		return info, nil
	}
	newProp := &requiredProperty{
		props:      make([]*columnProp, 0, len(prop.props)),
		sortKeyLen: prop.sortKeyLen,
		limit:      prop.limit}
	childSchema := p.GetChildByIndex(0).GetSchema()
	usedCols := make([]bool, len(childSchema))
	canPassSort := true
loop:
	for _, c := range prop.props {
		idx := p.schema.GetIndex(c.col)
		switch v := p.Exprs[idx].(type) {
		case *expression.Column:
			childIdx := childSchema.GetIndex(v)
			if !usedCols[childIdx] {
				usedCols[childIdx] = true
				newProp.props = append(newProp.props, &columnProp{col: v, desc: c.desc})
			}
		case *expression.ScalarFunction:
			newProp = nil
			canPassSort = false
			break loop
		default:
			newProp.sortKeyLen--
		}
	}
	if !canPassSort {
		return &physicalPlanInfo{cost: math.MaxFloat64}, nil
	}
	info, err = p.GetChildByIndex(0).(LogicalPlan).convert2PhysicalPlan(newProp)
	if err != nil {
		return nil, errors.Trace(err)
	}
	info = addPlanToResponse(p, info)
	p.storePlanInfo(prop, info)
	return info, nil
}

func matchProp(ctx context.Context, target, new *requiredProperty) bool {
	if target.sortKeyLen > len(new.props) {
		return false
	}
	for i := 0; i < target.sortKeyLen; i++ {
		if !target.props[i].equal(new.props[i], ctx) {
			return false
		}
	}
	for i := target.sortKeyLen; i < len(target.props); i++ {
		isMatch := false
		for _, pro := range new.props {
			if pro.col.Equal(target.props[i].col, ctx) {
				isMatch = true
				break
			}
		}
		if !isMatch {
			return false
		}
	}
	return true
}

// convert2PhysicalPlan implements the LogicalPlan convert2PhysicalPlan interface.
func (p *Sort) convert2PhysicalPlan(prop *requiredProperty) (*physicalPlanInfo, error) {
	info, err := p.getPlanInfo(prop)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if info != nil {
		return info, nil
	}
	selfProp := &requiredProperty{
		props: make([]*columnProp, 0, len(p.ByItems)),
	}
	for _, by := range p.ByItems {
		if col, ok := by.Expr.(*expression.Column); ok {
			selfProp.props = append(selfProp.props, &columnProp{col: col, desc: by.Desc})
		} else {
			selfProp.props = nil
			break
		}
	}
	selfProp.sortKeyLen = len(selfProp.props)
	if len(selfProp.props) != 0 && len(prop.props) == 0 && prop.limit != nil {
		selfProp.limit = prop.limit
	}
	sortedPlanInfo, err := p.GetChildByIndex(0).(LogicalPlan).convert2PhysicalPlan(selfProp)
	if err != nil {
		return nil, errors.Trace(err)
	}
	unSortedPlanInfo, err := p.GetChildByIndex(0).(LogicalPlan).convert2PhysicalPlan(&requiredProperty{})
	if err != nil {
		return nil, errors.Trace(err)
	}
	sortCost := sortCost(unSortedPlanInfo.count)
	if len(selfProp.props) == 0 {
		np := p.Copy().(*Sort)
		np.ExecLimit = prop.limit
		sortedPlanInfo = addPlanToResponse(np, sortedPlanInfo)
	} else if sortCost+unSortedPlanInfo.cost < sortedPlanInfo.cost {
		sortedPlanInfo.cost = sortCost + unSortedPlanInfo.cost
		np := *p
		np.ExecLimit = selfProp.limit
		sortedPlanInfo = addPlanToResponse(&np, unSortedPlanInfo)
	}
	if !matchProp(p.ctx, prop, selfProp) {
		sortedPlanInfo.cost = math.MaxFloat64
	}
	p.storePlanInfo(prop, sortedPlanInfo)
	return sortedPlanInfo, nil
}

// addCachePlan will add a Cache plan above the plan whose father's IsCorrelated() is true but its own IsCorrelated() is false.
func addCachePlan(p PhysicalPlan) PhysicalPlan {
	if len(p.GetChildren()) == 0 {
		return p
	}
	np := p
	newChildren := make([]Plan, 0, len(np.GetChildren()))
	for _, child := range p.GetChildren() {
		if child.IsCorrelated() {
			newChildren = append(newChildren, addCachePlan(child.(PhysicalPlan)))
		} else {
			newChild := &Cache{}
			addChild(newChild, child)
			newChild.SetSchema(child.GetSchema())
			newChild.SetParents(np)
			newChildren = append(newChildren, newChild)
		}
	}
	np.SetChildren(newChildren...)
	return np
}

// convert2PhysicalPlan implements the LogicalPlan convert2PhysicalPlan interface.
func (p *Apply) convert2PhysicalPlan(prop *requiredProperty) (*physicalPlanInfo, error) {
	info, err := p.getPlanInfo(prop)
	if err != nil {
		return info, errors.Trace(err)
	}
	if info != nil {
		return info, nil
	}
	innerPlan := p.children[1].(LogicalPlan)
	allFromOuter := true
	for _, col := range prop.props {
		if innerPlan.GetSchema().GetIndex(col.col) != -1 {
			allFromOuter = false
		}
	}
	if !allFromOuter {
		return &physicalPlanInfo{cost: math.MaxFloat64}, err
	}
	child := p.GetChildByIndex(0).(LogicalPlan)
	innerInfo, err := innerPlan.convert2PhysicalPlan(&requiredProperty{})
	innerInfo.p = addCachePlan(innerInfo.p)
	if err != nil {
		return nil, errors.Trace(err)
	}
	np := &PhysicalApply{
		OuterSchema: p.corCols,
		Checker:     p.Checker,
	}
	np.tp = "PhysicalApply"
	np.allocator = p.allocator
	np.initIDAndContext(p.ctx)
	np.correlated = p.IsCorrelated()
	np.SetSchema(p.GetSchema())
	limit := prop.limit
	info, err = child.convert2PhysicalPlan(removeLimit(prop))
	if err != nil {
		return nil, errors.Trace(err)
	}
	info = addPlanToResponse(np, info)
	addChild(info.p, innerInfo.p)
	info = enforceProperty(limitProperty(limit), info)
	p.storePlanInfo(prop, info)
	return info, nil
}

// convert2PhysicalPlan implements the LogicalPlan convert2PhysicalPlan interface.
// TODO: support streaming distinct.
func (p *Distinct) convert2PhysicalPlan(prop *requiredProperty) (*physicalPlanInfo, error) {
	info, err := p.getPlanInfo(prop)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if info != nil {
		return info, nil
	}
	child := p.GetChildByIndex(0).(LogicalPlan)
	limit := prop.limit
	info, err = child.convert2PhysicalPlan(removeLimit(prop))
	if err != nil {
		return nil, errors.Trace(err)
	}
	info = addPlanToResponse(p, info)
	info.count = uint64(float64(info.count) * distinctFactor)
	info = enforceProperty(limitProperty(limit), info)
	p.storePlanInfo(prop, info)
	return info, nil
}