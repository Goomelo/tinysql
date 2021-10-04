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

package expression

import (
	"fmt"
	"strings"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/parser/model"
	"github.com/pingcap/tidb/parser/mysql"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/sessionctx/stmtctx"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/util/chunk"
	"github.com/pingcap/tidb/util/codec"
)

// Column represents a column.
type Column struct {
	RetType *types.FieldType
	// ID is used to specify whether this column is ExtraHandleColumn or to access histogram.
	// We'll try to remove it in the future.
	ID int64
	// UniqueID is the unique id of this column.
	UniqueID int64

	// Index is used for execution, to tell the column's position in the given row.
	Index int

	hashcode []byte

	OrigName string
}

// Equal implements Expression interface.
func (col *Column) Equal(_ sessionctx.Context, expr Expression) bool {
	if newCol, ok := expr.(*Column); ok {
		return newCol.UniqueID == col.UniqueID
	}
	return false
}

// VecEvalInt evaluates this expression in a vectorized manner.
func (col *Column) VecEvalInt(ctx sessionctx.Context, input *chunk.Chunk, result *chunk.Column) error {
	if col.RetType.Hybrid() {
		it := chunk.NewIterator4Chunk(input)
		result.ResizeInt64(0, false)
		for row := it.Begin(); row != it.End(); row = it.Next() {
			v, null, err := col.EvalInt(ctx, row)
			if err != nil {
				return err
			}
			if null {
				result.AppendNull()
			} else {
				result.AppendInt64(v)
			}
		}
		return nil
	}
	input.Column(col.Index).CopyReconstruct(input.Sel(), result)
	return nil
}

// VecEvalReal evaluates this expression in a vectorized manner.
func (col *Column) VecEvalReal(ctx sessionctx.Context, input *chunk.Chunk, result *chunk.Column) error {
	n := input.NumRows()
	src := input.Column(col.Index)
	if col.GetType().Tp == mysql.TypeFloat {
		result.ResizeFloat64(n, false)
		f32s := src.Float32s()
		f64s := result.Float64s()
		sel := input.Sel()
		if sel != nil {
			for i, j := range sel {
				if src.IsNull(j) {
					result.SetNull(i, true)
				} else {
					f64s[i] = float64(f32s[j])
				}
			}
			return nil
		}
		for i := range f32s {
			// TODO(zhangyuanjia): speed up the way to manipulate null-bitmaps.
			if src.IsNull(i) {
				result.SetNull(i, true)
			} else {
				f64s[i] = float64(f32s[i])
			}
		}
		return nil
	}
	input.Column(col.Index).CopyReconstruct(input.Sel(), result)
	return nil
}

// VecEvalString evaluates this expression in a vectorized manner.
func (col *Column) VecEvalString(ctx sessionctx.Context, input *chunk.Chunk, result *chunk.Column) error {
	if col.RetType.Hybrid() || ctx.GetSessionVars().StmtCtx.PadCharToFullLength {
		it := chunk.NewIterator4Chunk(input)
		result.ReserveString(input.NumRows())
		for row := it.Begin(); row != it.End(); row = it.Next() {
			v, null, err := col.EvalString(ctx, row)
			if err != nil {
				return err
			}
			if null {
				result.AppendNull()
			} else {
				result.AppendString(v)
			}
		}
		return nil
	}
	input.Column(col.Index).CopyReconstruct(input.Sel(), result)
	return nil
}

const columnPrefix = "Column#"

// String implements Stringer interface.
func (col *Column) String() string {
	if col.OrigName != "" {
		return col.OrigName
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "%s%d", columnPrefix, col.UniqueID)
	return builder.String()
}

// MarshalJSON implements json.Marshaler interface.
func (col *Column) MarshalJSON() ([]byte, error) {
	return []byte(fmt.Sprintf("\"%s\"", col)), nil
}

// GetType implements Expression interface.
func (col *Column) GetType() *types.FieldType {
	return col.RetType
}

// Eval implements Expression interface.
func (col *Column) Eval(row chunk.Row) (types.Datum, error) {
	return row.GetDatum(col.Index, col.RetType), nil
}

// EvalInt returns int representation of Column.
func (col *Column) EvalInt(ctx sessionctx.Context, row chunk.Row) (int64, bool, error) {
	if col.GetType().Hybrid() {
		val := row.GetDatum(col.Index, col.RetType)
		if val.IsNull() {
			return 0, true, nil
		}
		res, err := val.ToInt64(ctx.GetSessionVars().StmtCtx)
		return res, err != nil, err
	}
	if row.IsNull(col.Index) {
		return 0, true, nil
	}
	return row.GetInt64(col.Index), false, nil
}

// EvalReal returns real representation of Column.
func (col *Column) EvalReal(ctx sessionctx.Context, row chunk.Row) (float64, bool, error) {
	if row.IsNull(col.Index) {
		return 0, true, nil
	}
	if col.GetType().Tp == mysql.TypeFloat {
		return float64(row.GetFloat32(col.Index)), false, nil
	}
	return row.GetFloat64(col.Index), false, nil
}

// EvalString returns string representation of Column.
func (col *Column) EvalString(ctx sessionctx.Context, row chunk.Row) (string, bool, error) {
	if row.IsNull(col.Index) {
		return "", true, nil
	}

	// Specially handle the ENUM/SET/BIT input value.
	if col.GetType().Hybrid() {
		val := row.GetDatum(col.Index, col.RetType)
		res, err := val.ToString()
		return res, err != nil, err
	}

	val := row.GetString(col.Index)
	if ctx.GetSessionVars().StmtCtx.PadCharToFullLength && col.GetType().Tp == mysql.TypeString {
		valLen := len([]rune(val))
		if valLen < col.RetType.Flen {
			val = val + strings.Repeat(" ", col.RetType.Flen-valLen)
		}
	}
	return val, false, nil
}

// Clone implements Expression interface.
func (col *Column) Clone() Expression {
	newCol := *col
	return &newCol
}

// IsCorrelated implements Expression interface.
func (col *Column) IsCorrelated() bool {
	return false
}

// ConstItem implements Expression interface.
func (col *Column) ConstItem() bool {
	return false
}

// Decorrelate implements Expression interface.
func (col *Column) Decorrelate(_ *Schema) Expression {
	return col
}

// HashCode implements Expression interface.
func (col *Column) HashCode(_ *stmtctx.StatementContext) []byte {
	if len(col.hashcode) != 0 {
		return col.hashcode
	}
	col.hashcode = make([]byte, 0, 9)
	col.hashcode = append(col.hashcode, columnFlag)
	col.hashcode = codec.EncodeInt(col.hashcode, int64(col.UniqueID))
	return col.hashcode
}

// ResolveIndices implements Expression interface.
func (col *Column) ResolveIndices(schema *Schema) (Expression, error) {
	newCol := col.Clone()
	err := newCol.resolveIndices(schema)
	return newCol, err
}

func (col *Column) resolveIndices(schema *Schema) error {
	col.Index = schema.ColumnIndex(col)
	if col.Index == -1 {
		return errors.Errorf("Can't find column %s in schema %s", col, schema)
	}
	return nil
}

// Vectorized returns if this expression supports vectorized evaluation.
func (col *Column) Vectorized() bool {
	return true
}

// ToInfo converts the expression.Column to model.ColumnInfo for casting values,
// beware it doesn't fill all the fields of the model.ColumnInfo.
func (col *Column) ToInfo() *model.ColumnInfo {
	return &model.ColumnInfo{
		ID:        col.ID,
		FieldType: *col.RetType,
	}
}

// Column2Exprs will transfer column slice to expression slice.
func Column2Exprs(cols []*Column) []Expression {
	result := make([]Expression, 0, len(cols))
	for _, col := range cols {
		result = append(result, col)
	}
	return result
}

// ColInfo2Col finds the corresponding column of the ColumnInfo in a column slice.
func ColInfo2Col(cols []*Column, col *model.ColumnInfo) *Column {
	for _, c := range cols {
		if c.ID == col.ID {
			return c
		}
	}
	return nil
}

// indexCol2Col finds the corresponding column of the IndexColumn in a column slice.
func indexCol2Col(colInfos []*model.ColumnInfo, cols []*Column, col *model.IndexColumn) *Column {
	for i, info := range colInfos {
		if info.Name.L == col.Name.L {
			return cols[i]
		}
	}
	return nil
}

// IndexInfo2PrefixCols gets the corresponding []*Column of the indexInfo's []*IndexColumn,
// together with a []int containing their lengths.
// If this index has three IndexColumn that the 1st and 3rd IndexColumn has corresponding *Column,
// the return value will be only the 1st corresponding *Column and its length.
// TODO: Use a struct to represent {*Column, int}. And merge IndexInfo2PrefixCols and IndexInfo2Cols.
func IndexInfo2PrefixCols(colInfos []*model.ColumnInfo, cols []*Column, index *model.IndexInfo) ([]*Column, []int) {
	retCols := make([]*Column, 0, len(index.Columns))
	lengths := make([]int, 0, len(index.Columns))
	for _, c := range index.Columns {
		col := indexCol2Col(colInfos, cols, c)
		if col == nil {
			return retCols, lengths
		}
		retCols = append(retCols, col)
		if c.Length != types.UnspecifiedLength && c.Length == col.RetType.Flen {
			lengths = append(lengths, types.UnspecifiedLength)
		} else {
			lengths = append(lengths, c.Length)
		}
	}
	return retCols, lengths
}

// IndexInfo2Cols gets the corresponding []*Column of the indexInfo's []*IndexColumn,
// together with a []int containing their lengths.
// If this index has three IndexColumn that the 1st and 3rd IndexColumn has corresponding *Column,
// the return value will be [col1, nil, col2].
func IndexInfo2Cols(colInfos []*model.ColumnInfo, cols []*Column, index *model.IndexInfo) ([]*Column, []int) {
	retCols := make([]*Column, 0, len(index.Columns))
	lens := make([]int, 0, len(index.Columns))
	for _, c := range index.Columns {
		col := indexCol2Col(colInfos, cols, c)
		if col == nil {
			retCols = append(retCols, col)
			lens = append(lens, types.UnspecifiedLength)
			continue
		}
		retCols = append(retCols, col)
		if c.Length != types.UnspecifiedLength && c.Length == col.RetType.Flen {
			lens = append(lens, types.UnspecifiedLength)
		} else {
			lens = append(lens, c.Length)
		}
	}
	return retCols, lens
}

// FindPrefixOfIndex will find columns in index by checking the unique id.
// So it will return at once no matching column is found.
func FindPrefixOfIndex(cols []*Column, idxColIDs []int64) []*Column {
	retCols := make([]*Column, 0, len(idxColIDs))
idLoop:
	for _, id := range idxColIDs {
		for _, col := range cols {
			if col.UniqueID == id {
				retCols = append(retCols, col)
				continue idLoop
			}
		}
		// If no matching column is found, just return.
		return retCols
	}
	return retCols
}