// Copyright 2016 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package sql

import (
	"context"
	"sort"
	"strconv"

	"github.com/pkg/errors"

	"github.com/cockroachdb/cockroach/pkg/security"
	"github.com/cockroachdb/cockroach/pkg/sql/privilege"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sessiondata"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
)

const (
	informationSchemaName = "information_schema"
	pgCatalogName         = sessiondata.PgCatalogName
)

var informationSchema = virtualSchema{
	name: informationSchemaName,
	tables: []virtualSchemaTable{
		informationSchemaColumnPrivileges,
		informationSchemaColumnsTable,
		informationSchemaKeyColumnUsageTable,
		informationSchemaReferentialConstraintsTable,
		informationSchemaSchemataTable,
		informationSchemaSchemataTablePrivileges,
		informationSchemaSequences,
		informationSchemaStatisticsTable,
		informationSchemaTableConstraintTable,
		informationSchemaTablePrivileges,
		informationSchemaTablesTable,
		informationSchemaViewsTable,
		informationSchemaUserPrivileges,
	},
	tableValidator: validateInformationSchemaTable,
}

var (
	// defString is used as the value for columns included in the sql standard
	// of information_schema that don't make sense for CockroachDB. This is
	// identical to the behavior of MySQL.
	defString = tree.NewDString("def")

	// information_schema was defined before the BOOLEAN data type was added to
	// the SQL specification. Because of this, boolean values are represented as
	// STRINGs. The BOOLEAN data type should NEVER be used in information_schema
	// tables. Instead, define columns as STRINGs and map bools to STRINGs using
	// yesOrNoDatum.
	yesString = tree.NewDString("YES")
	noString  = tree.NewDString("NO")
)

func yesOrNoDatum(b bool) tree.Datum {
	if b {
		return yesString
	}
	return noString
}

func dStringOrNull(s string) tree.Datum {
	if s == "" {
		return tree.DNull
	}
	return tree.NewDString(s)
}

func dNameOrNull(s string) tree.Datum {
	if s == "" {
		return tree.DNull
	}
	return tree.NewDName(s)
}

func dStringPtrOrNull(s *string) tree.Datum {
	if s == nil {
		return tree.DNull
	}
	return tree.NewDString(*s)
}

func dIntFnOrNull(fn func() (int32, bool)) tree.Datum {
	if n, ok := fn(); ok {
		return tree.NewDInt(tree.DInt(n))
	}
	return tree.DNull
}

func validateInformationSchemaTable(table *sqlbase.TableDescriptor) error {
	// Make sure no tables have boolean columns.
	for _, col := range table.Columns {
		if col.Type.SemanticType == sqlbase.ColumnType_BOOL {
			return errors.Errorf("information_schema tables should never use BOOL columns. "+
				"See the comment about yesOrNoDatum. Found BOOL column in %s.", table.Name)
		}
	}
	return nil
}

// Postgres: https://www.postgresql.org/docs/9.6/static/infoschema-column-privileges.html
// MySQL:    https://dev.mysql.com/doc/refman/5.7/en/column-privileges-table.html
var informationSchemaColumnPrivileges = virtualSchemaTable{
	schema: `
CREATE TABLE information_schema.column_privileges (
	GRANTOR STRING NOT NULL,
	GRANTEE STRING NOT NULL,
	TABLE_CATALOG STRING NOT NULL,
	TABLE_SCHEMA STRING NOT NULL,
	TABLE_NAME STRING NOT NULL,
	COLUMN_NAME STRING NOT NULL,
	PRIVILEGE_TYPE STRING NOT NULL,
	IS_GRANTABLE STRING NOT NULL
);
`,
	populate: func(ctx context.Context, p *planner, prefix string, addRow func(...tree.Datum) error) error {
		return forEachTableDesc(ctx, p, prefix, func(db *sqlbase.DatabaseDescriptor, table *sqlbase.TableDescriptor) error {
			columndata := privilege.List{privilege.SELECT, privilege.INSERT, privilege.UPDATE} // privileges for column level granularity
			for _, u := range table.Privileges.Users {
				for _, priv := range columndata {
					if priv.Mask()&u.Privileges != 0 {
						for _, cd := range table.Columns {
							if err := addRow(
								tree.DNull,                     // grantor
								tree.NewDString(u.User),        // grantee
								defString,                      // table_catalog
								tree.NewDString(db.Name),       // table_schema
								tree.NewDString(table.Name),    // table_name
								tree.NewDString(cd.Name),       // column_name
								tree.NewDString(priv.String()), // privilege_type
								tree.DNull,                     // is_grantable
							); err != nil {
								return err
							}
						}
					}
				}
			}
			return nil
		})
	},
}

// Postgres: https://www.postgresql.org/docs/9.6/static/infoschema-columns.html
// MySQL:    https://dev.mysql.com/doc/refman/5.7/en/columns-table.html
var informationSchemaColumnsTable = virtualSchemaTable{
	schema: `
CREATE TABLE information_schema.columns (
	TABLE_CATALOG STRING NOT NULL,
	TABLE_SCHEMA STRING NOT NULL,
	TABLE_NAME STRING NOT NULL,
	COLUMN_NAME STRING NOT NULL,
	ORDINAL_POSITION INT NOT NULL,
	COLUMN_DEFAULT STRING,
	IS_NULLABLE STRING NOT NULL,
	DATA_TYPE STRING NOT NULL,
	CHARACTER_MAXIMUM_LENGTH INT,
	CHARACTER_OCTET_LENGTH INT,
	NUMERIC_PRECISION INT,
	NUMERIC_SCALE INT,
	DATETIME_PRECISION INT,
	CHARACTER_SET_CATALOG STRING,
	CHARACTER_SET_SCHEMA STRING,
	CHARACTER_SET_NAME STRING
);
`,
	populate: func(ctx context.Context, p *planner, prefix string, addRow func(...tree.Datum) error) error {
		return forEachTableDesc(ctx, p, prefix, func(db *sqlbase.DatabaseDescriptor, table *sqlbase.TableDescriptor) error {
			// Table descriptors already holds columns in-order.
			visible := 0
			return forEachColumnInTable(table, func(column *sqlbase.ColumnDescriptor) error {
				visible++
				return addRow(
					defString,                                // table_catalog
					tree.NewDString(db.Name),                 // table_schema
					tree.NewDString(table.Name),              // table_name
					tree.NewDString(column.Name),             // column_name
					tree.NewDInt(tree.DInt(visible)),         // ordinal_position, 1-indexed
					dStringPtrOrNull(column.DefaultExpr),     // column_default
					yesOrNoDatum(column.Nullable),            // is_nullable
					tree.NewDString(column.Type.SQLString()), // data_type
					characterMaximumLength(column.Type),      // character_maximum_length
					characterOctetLength(column.Type),        // character_octet_length
					numericPrecision(column.Type),            // numeric_precision
					numericScale(column.Type),                // numeric_scale
					datetimePrecision(column.Type),           // datetime_precision
					tree.DNull,                               // character_set_catalog
					tree.DNull,                               // character_set_schema
					tree.DNull,                               // character_set_name
				)
			})
		})
	},
}

func characterMaximumLength(colType sqlbase.ColumnType) tree.Datum {
	return dIntFnOrNull(colType.MaxCharacterLength)
}

func characterOctetLength(colType sqlbase.ColumnType) tree.Datum {
	return dIntFnOrNull(colType.MaxOctetLength)
}

func numericPrecision(colType sqlbase.ColumnType) tree.Datum {
	return dIntFnOrNull(colType.NumericPrecision)
}

func numericScale(colType sqlbase.ColumnType) tree.Datum {
	return dIntFnOrNull(colType.NumericScale)
}

func datetimePrecision(colType sqlbase.ColumnType) tree.Datum {
	// We currently do not support a datetime precision.
	return tree.DNull
}

// Postgres: https://www.postgresql.org/docs/9.6/static/infoschema-key-column-usage.html
// MySQL:    https://dev.mysql.com/doc/refman/5.7/en/key-column-usage-table.html
var informationSchemaKeyColumnUsageTable = virtualSchemaTable{
	schema: `
CREATE TABLE information_schema.key_column_usage (
	CONSTRAINT_CATALOG STRING,
	CONSTRAINT_SCHEMA STRING,
	CONSTRAINT_NAME STRING,
	TABLE_CATALOG STRING NOT NULL,
	TABLE_SCHEMA STRING NOT NULL,
	TABLE_NAME STRING NOT NULL,
	COLUMN_NAME STRING NOT NULL,
	ORDINAL_POSITION INT NOT NULL,
	POSITION_IN_UNIQUE_CONSTRAINT INT
);`,
	populate: func(ctx context.Context, p *planner, prefix string, addRow func(...tree.Datum) error) error {
		return forEachTableDescWithTableLookup(ctx, p, prefix, func(
			db *sqlbase.DatabaseDescriptor,
			table *sqlbase.TableDescriptor,
			tableLookup tableLookupFn,
		) error {
			info, err := table.GetConstraintInfoWithLookup(tableLookup.tableOrErr)
			if err != nil {
				return err
			}

			for name, c := range info {
				// Only Primary Key, Foreign Key, and Unique constraints are included.
				switch c.Kind {
				case sqlbase.ConstraintTypePK:
				case sqlbase.ConstraintTypeFK:
				case sqlbase.ConstraintTypeUnique:
				default:
					continue
				}

				for pos, column := range c.Columns {
					ordinalPos := tree.NewDInt(tree.DInt(pos + 1))
					uniquePos := tree.DNull
					if c.Kind == sqlbase.ConstraintTypeFK {
						uniquePos = ordinalPos
					}
					if err := addRow(
						defString,                   // constraint_catalog
						tree.NewDString(db.Name),    // constraint_schema
						dStringOrNull(name),         // constraint_name
						defString,                   // table_catalog
						tree.NewDString(db.Name),    // table_schema
						tree.NewDString(table.Name), // table_name
						tree.NewDString(column),     // column_name
						ordinalPos,                  // ordinal_position, 1-indexed
						uniquePos,                   // position_in_unique_constraint
					); err != nil {
						return err
					}
				}
			}
			return nil
		})
	},
}

var (
	matchOptionFull    = tree.NewDString("FULL")
	matchOptionPartial = tree.NewDString("PARTIAL")
	matchOptionNone    = tree.NewDString("NONE")

	// Avoid unused warning for constants.
	_ = matchOptionPartial
	_ = matchOptionNone

	refConstraintRuleNoAction   = tree.NewDString("NO ACTION")
	refConstraintRuleRestrict   = tree.NewDString("RESTRICT")
	refConstraintRuleSetNull    = tree.NewDString("SET NULL")
	refConstraintRuleSetDefault = tree.NewDString("SET DEFAULT")
	refConstraintRuleCascade    = tree.NewDString("CASCADE")
)

func dStringForFKAction(action sqlbase.ForeignKeyReference_Action) tree.Datum {
	switch action {
	case sqlbase.ForeignKeyReference_NO_ACTION:
		return refConstraintRuleNoAction
	case sqlbase.ForeignKeyReference_RESTRICT:
		return refConstraintRuleRestrict
	case sqlbase.ForeignKeyReference_SET_NULL:
		return refConstraintRuleSetNull
	case sqlbase.ForeignKeyReference_SET_DEFAULT:
		return refConstraintRuleSetDefault
	case sqlbase.ForeignKeyReference_CASCADE:
		return refConstraintRuleCascade
	}
	panic(errors.Errorf("unexpected ForeignKeyReference_Action: %v", action))
}

// Postgres: https://www.postgresql.org/docs/9.6/static/infoschema-referential-constraints.html
// MySQL:    https://dev.mysql.com/doc/refman/5.7/en/referential-constraints-table.html
var informationSchemaReferentialConstraintsTable = virtualSchemaTable{
	schema: `
CREATE TABLE information_schema.referential_constraints (
	CONSTRAINT_CATALOG STRING NOT NULL,
	CONSTRAINT_SCHEMA STRING NOT NULL,
	CONSTRAINT_NAME STRING NOT NULL,
	UNIQUE_CONSTRAINT_CATALOG STRING NOT NULL,
	UNIQUE_CONSTRAINT_SCHEMA STRING NOT NULL,
	UNIQUE_CONSTRAINT_NAME STRING,
	MATCH_OPTION STRING NOT NULL,
	UPDATE_RULE STRING NOT NULL,
	DELETE_RULE STRING NOT NULL,
	TABLE_NAME STRING NOT NULL,
	REFERENCED_TABLE_NAME STRING NOT NULL
);`,
	populate: func(ctx context.Context, p *planner, prefix string, addRow func(...tree.Datum) error) error {
		return forEachTableDescWithTableLookup(ctx, p, prefix, func(
			db *sqlbase.DatabaseDescriptor,
			table *sqlbase.TableDescriptor,
			tableLookup tableLookupFn,
		) error {
			return forEachIndexInTable(table, func(index *sqlbase.IndexDescriptor) error {
				fk := index.ForeignKey
				if !fk.IsSet() {
					return nil
				}

				refTable, err := tableLookup.tableOrErr(fk.Table)
				if err != nil {
					return err
				}
				refIndex, err := refTable.FindIndexByID(fk.Index)
				if err != nil {
					return err
				}

				return addRow(
					defString,                       // constraint_catalog
					tree.NewDString(db.Name),        // constraint_schema
					tree.NewDString(fk.Name),        // constraint_name
					defString,                       // unique_constraint_catalog
					tree.NewDString(db.Name),        // unique_constraint_schema
					tree.NewDString(refIndex.Name),  // unique_constraint_name
					matchOptionFull,                 // match_option
					dStringForFKAction(fk.OnUpdate), // update_rule
					dStringForFKAction(fk.OnDelete), // delete_rule
					tree.NewDString(table.Name),     // table_name
					tree.NewDString(refTable.Name),  // referenced_table_name
				)
			})
		})
	},
}

// Postgres: https://www.postgresql.org/docs/9.6/static/infoschema-schemata.html
// MySQL:    https://dev.mysql.com/doc/refman/5.7/en/schemata-table.html
var informationSchemaSchemataTable = virtualSchemaTable{
	schema: `
CREATE TABLE information_schema.schemata (
	CATALOG_NAME STRING NOT NULL,
	SCHEMA_NAME STRING NOT NULL,
	DEFAULT_CHARACTER_SET_NAME STRING NOT NULL,
	SQL_PATH STRING
);`,
	populate: func(ctx context.Context, p *planner, _ string, addRow func(...tree.Datum) error) error {
		return forEachDatabaseDesc(ctx, p, func(db *sqlbase.DatabaseDescriptor) error {
			return addRow(
				defString,                // catalog_name
				tree.NewDString(db.Name), // schema_name
				tree.DNull,               // default_character_set_name
				tree.DNull,               // sql_path
			)
		})
	},
}

// Postgres: missing
// MySQL:    https://dev.mysql.com/doc/refman/5.7/en/schema-privileges-table.html
var informationSchemaSchemataTablePrivileges = virtualSchemaTable{
	schema: `
CREATE TABLE information_schema.schema_privileges (
	GRANTEE STRING NOT NULL,
	TABLE_CATALOG STRING NOT NULL,
	TABLE_SCHEMA STRING NOT NULL,
	PRIVILEGE_TYPE STRING NOT NULL,
	IS_GRANTABLE STRING NOT NULL
);
`,
	populate: func(ctx context.Context, p *planner, _ string, addRow func(...tree.Datum) error) error {
		return forEachDatabaseDesc(ctx, p, func(db *sqlbase.DatabaseDescriptor) error {
			for _, u := range db.Privileges.Show() {
				for _, priv := range u.Privileges {
					if err := addRow(
						tree.NewDString(u.User),  // grantee
						defString,                // table_catalog
						tree.NewDString(db.Name), // table_schema
						tree.NewDString(priv),    // privilege_type
						tree.DNull,               // is_grantable
					); err != nil {
						return err
					}
				}
			}
			return nil
		})
	},
}

var (
	indexDirectionNA   = tree.NewDString("N/A")
	indexDirectionAsc  = tree.NewDString(sqlbase.IndexDescriptor_ASC.String())
	indexDirectionDesc = tree.NewDString(sqlbase.IndexDescriptor_DESC.String())
)

func dStringForIndexDirection(dir sqlbase.IndexDescriptor_Direction) tree.Datum {
	switch dir {
	case sqlbase.IndexDescriptor_ASC:
		return indexDirectionAsc
	case sqlbase.IndexDescriptor_DESC:
		return indexDirectionDesc
	}
	panic("unreachable")
}

// Postgres: https://www.postgresql.org/docs/9.6/static/infoschema-sequences.html
// MySQL:    missing
var informationSchemaSequences = virtualSchemaTable{
	schema: `
CREATE TABLE information_schema.sequences (
    SEQUENCE_CATALOG STRING NOT NULL,
    SEQUENCE_SCHEMA STRING NOT NULL,
    SEQUENCE_NAME STRING NOT NULL,
    DATA_TYPE STRING NOT NULL,
    NUMERIC_PRECISION INT NOT NULL,
    NUMERIC_PRECISION_RADIX INT NOT NULL,
    NUMERIC_SCALE INT NOT NULL,
    START_VALUE STRING NOT NULL,
    MINIMUM_VALUE STRING NOT NULL,
    MAXIMUM_VALUE STRING NOT NULL,
    INCREMENT STRING NOT NULL,
    CYCLE_OPTION STRING NOT NULL
);`,
	populate: func(ctx context.Context, p *planner, prefix string, addRow func(...tree.Datum) error) error {
		return forEachTableDesc(ctx, p, prefix, func(db *sqlbase.DatabaseDescriptor, table *sqlbase.TableDescriptor) error {
			if !table.IsSequence() {
				return nil
			}
			return addRow(
				defString,                        // catalog
				tree.NewDString(db.GetName()),    // schema
				tree.NewDString(table.GetName()), // name
				tree.NewDString("INT"),           // type
				tree.NewDInt(64),                 // numeric precision
				tree.NewDInt(2),                  // numeric precision radix
				tree.NewDInt(0),                  // numeric scale
				tree.NewDString(strconv.FormatInt(table.SequenceOpts.Start, 10)),     // start value
				tree.NewDString(strconv.FormatInt(table.SequenceOpts.MinValue, 10)),  // min value
				tree.NewDString(strconv.FormatInt(table.SequenceOpts.MaxValue, 10)),  // max value
				tree.NewDString(strconv.FormatInt(table.SequenceOpts.Increment, 10)), // increment
				noString, // cycle
			)
		})
	},
}

// Postgres: missing
// MySQL:    https://dev.mysql.com/doc/refman/5.7/en/statistics-table.html
var informationSchemaStatisticsTable = virtualSchemaTable{
	schema: `
CREATE TABLE information_schema.statistics (
	TABLE_CATALOG STRING NOT NULL,
	TABLE_SCHEMA STRING NOT NULL,
	TABLE_NAME STRING NOT NULL,
	NON_UNIQUE STRING NOT NULL,
	INDEX_SCHEMA STRING NOT NULL,
	INDEX_NAME STRING NOT NULL,
	SEQ_IN_INDEX INT NOT NULL,
	COLUMN_NAME STRING NOT NULL,
	"COLLATION" STRING NOT NULL,
	CARDINALITY INT NOT NULL,
	DIRECTION STRING NOT NULL,
	STORING STRING NOT NULL,
	IMPLICIT STRING NOT NULL
);`,
	populate: func(ctx context.Context, p *planner, prefix string, addRow func(...tree.Datum) error) error {
		return forEachTableDesc(ctx, p, prefix, func(db *sqlbase.DatabaseDescriptor, table *sqlbase.TableDescriptor) error {
			appendRow := func(index *sqlbase.IndexDescriptor, colName string, sequence int,
				direction tree.Datum, isStored, isImplicit bool,
			) error {
				return addRow(
					defString,                         // table_catalog
					tree.NewDString(db.GetName()),     // table_schema
					tree.NewDString(table.GetName()),  // table_name
					yesOrNoDatum(!index.Unique),       // non_unique
					tree.NewDString(db.GetName()),     // index_schema
					tree.NewDString(index.Name),       // index_name
					tree.NewDInt(tree.DInt(sequence)), // seq_in_index
					tree.NewDString(colName),          // column_name
					tree.DNull,                        // collation
					tree.DNull,                        // cardinality
					direction,                         // direction
					yesOrNoDatum(isStored),            // storing
					yesOrNoDatum(isImplicit),          // implicit
				)
			}

			return forEachIndexInTable(table, func(index *sqlbase.IndexDescriptor) error {
				// Columns in the primary key that aren't in index.ColumnNames or
				// index.StoreColumnNames are implicit columns in the index.
				var implicitCols map[string]struct{}
				var hasImplicitCols bool
				if index.HasOldStoredColumns() {
					// Old STORING format: implicit columns are extra columns minus stored
					// columns.
					hasImplicitCols = len(index.ExtraColumnIDs) > len(index.StoreColumnNames)
				} else {
					// New STORING format: implicit columns are extra columns.
					hasImplicitCols = len(index.ExtraColumnIDs) > 0
				}
				if hasImplicitCols {
					implicitCols = make(map[string]struct{})
					for _, col := range table.PrimaryIndex.ColumnNames {
						implicitCols[col] = struct{}{}
					}
				}

				sequence := 1
				for i, col := range index.ColumnNames {
					// We add a row for each column of index.
					dir := dStringForIndexDirection(index.ColumnDirections[i])
					if err := appendRow(index, col, sequence, dir, false, false); err != nil {
						return err
					}
					sequence++
					delete(implicitCols, col)
				}
				for _, col := range index.StoreColumnNames {
					// We add a row for each stored column of index.
					if err := appendRow(index, col, sequence,
						indexDirectionNA, true, false); err != nil {
						return err
					}
					sequence++
					delete(implicitCols, col)
				}
				for col := range implicitCols {
					// We add a row for each implicit column of index.
					if err := appendRow(index, col, sequence,
						indexDirectionAsc, false, true); err != nil {
						return err
					}
					sequence++
				}
				return nil
			})
		})
	},
}

// Postgres: https://www.postgresql.org/docs/9.6/static/infoschema-table-constraints.html
// MySQL:    https://dev.mysql.com/doc/refman/5.7/en/table-constraints-table.html
var informationSchemaTableConstraintTable = virtualSchemaTable{
	schema: `
CREATE TABLE information_schema.table_constraints (
	CONSTRAINT_CATALOG STRING NOT NULL,
	CONSTRAINT_SCHEMA STRING NOT NULL,
	CONSTRAINT_NAME STRING NOT NULL,
	TABLE_CATALOG STRING NOT NULL,
	TABLE_SCHEMA STRING NOT NULL,
	TABLE_NAME STRING NOT NULL,
	CONSTRAINT_TYPE STRING NOT NULL,
	IS_DEFERRABLE STRING NOT NULL,
	INITIALLY_DEFERRED STRING NOT NULL
);`,
	populate: func(ctx context.Context, p *planner, prefix string, addRow func(...tree.Datum) error) error {
		return forEachTableDescWithTableLookup(ctx, p, prefix, func(
			db *sqlbase.DatabaseDescriptor,
			table *sqlbase.TableDescriptor,
			tableLookup tableLookupFn,
		) error {
			info, err := table.GetConstraintInfoWithLookup(tableLookup.tableOrErr)
			if err != nil {
				return err
			}

			for name, c := range info {
				if err := addRow(
					defString,                       // constraint_catalog
					tree.NewDString(db.Name),        // constraint_schema
					dStringOrNull(name),             // constraint_name
					defString,                       // table_catalog
					tree.NewDString(db.Name),        // table_schema
					tree.NewDString(table.Name),     // table_name
					tree.NewDString(string(c.Kind)), // constraint_type
					yesOrNoDatum(false),             // is_deferrable
					yesOrNoDatum(false),             // initially_deferred
				); err != nil {
					return err
				}
			}
			return nil
		})
	},
}

// Postgres: missing
// MySQL:    https://dev.mysql.com/doc/refman/5.7/en/user-privileges-table.html
var informationSchemaUserPrivileges = virtualSchemaTable{
	schema: `
CREATE TABLE information_schema.user_privileges (
	GRANTEE STRING NOT NULL,
	TABLE_CATALOG STRING NOT NULL,
	PRIVILEGE_TYPE STRING NOT NULL,
	IS_GRANTABLE STRING NOT NULL
);`,
	populate: func(ctx context.Context, p *planner, _ string, addRow func(...tree.Datum) error) error {
		for _, u := range []string{security.RootUser, sqlbase.AdminRole} {
			grantee := tree.NewDString(u)
			for _, p := range privilege.List(privilege.ByValue[:]).SortedNames() {
				if err := addRow(
					grantee,            // grantee
					defString,          // table_catalog
					tree.NewDString(p), // privilege_type
					tree.DNull,         // is_grantable
				); err != nil {
					return err
				}
			}
		}
		return nil
	},
}

// Postgres: https://www.postgresql.org/docs/9.6/static/infoschema-table-privileges.html
// MySQL:    https://dev.mysql.com/doc/refman/5.7/en/table-privileges-table.html
var informationSchemaTablePrivileges = virtualSchemaTable{
	schema: `
CREATE TABLE information_schema.table_privileges (
	GRANTOR STRING NOT NULL,
	GRANTEE STRING NOT NULL,
	TABLE_CATALOG STRING NOT NULL,
	TABLE_SCHEMA STRING NOT NULL,
	TABLE_NAME STRING NOT NULL,
	PRIVILEGE_TYPE STRING NOT NULL,
	IS_GRANTABLE STRING NOT NULL,
	WITH_HIERARCHY STRING NOT NULL
);
`,
	populate: func(ctx context.Context, p *planner, prefix string, addRow func(...tree.Datum) error) error {
		return forEachTableDesc(ctx, p, prefix, func(db *sqlbase.DatabaseDescriptor, table *sqlbase.TableDescriptor) error {
			for _, u := range table.Privileges.Show() {
				for _, priv := range u.Privileges {
					if err := addRow(
						tree.DNull,                  // grantor
						tree.NewDString(u.User),     // grantee
						defString,                   // table_catalog
						tree.NewDString(db.Name),    // table_schema
						tree.NewDString(table.Name), // table_name
						tree.NewDString(priv),       // privilege_type
						tree.DNull,                  // is_grantable
						tree.DNull,                  // with_hierarchy
					); err != nil {
						return err
					}
				}
			}
			return nil
		})
	},
}

var (
	tableTypeSystemView = tree.NewDString("SYSTEM VIEW")
	tableTypeBaseTable  = tree.NewDString("BASE TABLE")
	tableTypeView       = tree.NewDString("VIEW")
)

// Postgres: https://www.postgresql.org/docs/9.6/static/infoschema-tables.html
// MySQL:    https://dev.mysql.com/doc/refman/5.7/en/tables-table.html
var informationSchemaTablesTable = virtualSchemaTable{
	schema: `
CREATE TABLE information_schema.tables (
	TABLE_CATALOG STRING NOT NULL,
	TABLE_SCHEMA STRING NOT NULL,
	TABLE_NAME STRING NOT NULL,
	TABLE_TYPE STRING NOT NULL,
	VERSION INT
);`,
	populate: func(ctx context.Context, p *planner, prefix string, addRow func(...tree.Datum) error) error {
		return forEachTableDesc(ctx, p, prefix, func(db *sqlbase.DatabaseDescriptor, table *sqlbase.TableDescriptor) error {
			if table.IsSequence() {
				return nil
			}
			tableType := tableTypeBaseTable
			if isVirtualDescriptor(table) {
				tableType = tableTypeSystemView
			} else if table.IsView() {
				tableType = tableTypeView
			}
			return addRow(
				defString,                   // table_catalog
				tree.NewDString(db.Name),    // table_schema
				tree.NewDString(table.Name), // table_name
				tableType,                   // table_type
				tree.NewDInt(tree.DInt(table.Version)), // version
			)
		})
	},
}

// Postgres: https://www.postgresql.org/docs/9.6/static/infoschema-views.html
// MySQL:    https://dev.mysql.com/doc/refman/5.7/en/views-table.html
var informationSchemaViewsTable = virtualSchemaTable{
	schema: `
CREATE TABLE information_schema.views (
    TABLE_CATALOG STRING NOT NULL,
    TABLE_SCHEMA STRING NOT NULL,
    TABLE_NAME STRING NOT NULL,
    VIEW_DEFINITION STRING NOT NULL,
    CHECK_OPTION STRING NOT NULL,
    IS_UPDATABLE STRING NOT NULL,
    IS_INSERTABLE_INTO STRING NOT NULL,
    IS_TRIGGER_UPDATABLE STRING NOT NULL,
    IS_TRIGGER_DELETABLE STRING NOT NULL,
    IS_TRIGGER_INSERTABLE_INTO STRING NOT NULL
);`,
	populate: func(ctx context.Context, p *planner, prefix string, addRow func(...tree.Datum) error) error {
		return forEachTableDesc(ctx, p, prefix, func(db *sqlbase.DatabaseDescriptor, table *sqlbase.TableDescriptor) error {
			if !table.IsView() {
				return nil
			}
			// Note that the view query printed will not include any column aliases
			// specified outside the initial view query into the definition returned,
			// unlike Postgres. For example, for the view created via
			//  `CREATE VIEW (a) AS SELECT b FROM foo`
			// we'll only print `SELECT b FROM foo` as the view definition here,
			// while Postgres would more accurately print `SELECT b AS a FROM foo`.
			// TODO(a-robinson): Insert column aliases into view query once we
			// have a semantic query representation to work with (#10083).
			return addRow(
				defString,                        // table_catalog
				tree.NewDString(db.Name),         // table_schema
				tree.NewDString(table.Name),      // table_name
				tree.NewDString(table.ViewQuery), // view_definition
				tree.DNull,                       // check_option
				tree.DNull,                       // is_updatable
				tree.DNull,                       // is_insertable_into
				tree.DNull,                       // is_trigger_updatable
				tree.DNull,                       // is_trigger_deletable
				tree.DNull,                       // is_trigger_insertable_into
			)
		})
	},
}

type sortedDBDescs []*sqlbase.DatabaseDescriptor

// sortedDBDescs implements sort.Interface. It sorts a slice of DatabaseDescriptors
// by the database name.
func (dbs sortedDBDescs) Len() int           { return len(dbs) }
func (dbs sortedDBDescs) Swap(i, j int)      { dbs[i], dbs[j] = dbs[j], dbs[i] }
func (dbs sortedDBDescs) Less(i, j int) bool { return dbs[i].Name < dbs[j].Name }

// forEachTableDesc retrieves all database descriptors and iterates through them in
// lexicographical order with respect to their name. For each database, the function
// will call fn with its descriptor.
func forEachDatabaseDesc(
	ctx context.Context, p *planner, fn func(*sqlbase.DatabaseDescriptor) error,
) error {
	// Handle real schemas.
	descs, err := p.Tables().getAllDescriptors(ctx, p.txn)
	if err != nil {
		return err
	}

	// Ignore table descriptors.
	var dbDescs []*sqlbase.DatabaseDescriptor
	for _, desc := range descs {
		if dbDesc, ok := desc.(*sqlbase.DatabaseDescriptor); ok {
			dbDescs = append(dbDescs, dbDesc)
		}
	}

	// Handle virtual schemas.
	for _, schema := range p.getVirtualTabler().getEntries() {
		dbDescs = append(dbDescs, schema.desc)
	}

	sort.Sort(sortedDBDescs(dbDescs))
	for _, db := range dbDescs {
		if userCanSeeDatabase(ctx, p, db) {
			if err := fn(db); err != nil {
				return err
			}
		}
	}
	return nil
}

// forEachTableDesc retrieves all table descriptors from the current database
// and all system databases and iterates through them in lexicographical order
// with respect primarily to database name and secondarily to table name. For
// each table, the function will call fn with its respective database and table
// descriptor.
//
// The prefix argument specifies in which database context we are
// requesting the descriptors. In context "" all descriptors are
// visible, in non-empty contexts only the descriptors of that
// database are visible.
func forEachTableDesc(
	ctx context.Context,
	p *planner,
	prefix string,
	fn func(*sqlbase.DatabaseDescriptor, *sqlbase.TableDescriptor) error,
) error {
	return forEachTableDescWithTableLookup(ctx, p, prefix, func(
		db *sqlbase.DatabaseDescriptor,
		table *sqlbase.TableDescriptor,
		_ tableLookupFn,
	) error {
		return fn(db, table)
	})
}

// forEachTableDescAll does the same as forEachTableDesc but also
// includes newly added non-public descriptors.
func forEachTableDescAll(
	ctx context.Context,
	p *planner,
	prefix string,
	fn func(*sqlbase.DatabaseDescriptor, *sqlbase.TableDescriptor) error,
) error {
	return forEachTableDescWithTableLookupInternal(ctx, p, prefix, true /* allowAdding */, func(
		db *sqlbase.DatabaseDescriptor,
		table *sqlbase.TableDescriptor,
		_ tableLookupFn,
	) error {
		return fn(db, table)
	})
}

// tableLookupFn can be used to retrieve a table descriptor and its corresponding
// database descriptor using the table's ID. Both descriptors will be nil if a
// table with the provided ID was not found.
type tableLookupFn func(tableID sqlbase.ID) (*sqlbase.DatabaseDescriptor, *sqlbase.TableDescriptor)

func (fn tableLookupFn) tableOrErr(id sqlbase.ID) (*sqlbase.TableDescriptor, error) {
	if _, t := fn(id); t != nil {
		return t, nil
	}
	return nil, errors.Errorf("could not find referenced table with ID %v", id)
}

// isSystemDatabaseName returns true if the input name is not a user db name.
func isSystemDatabaseName(name string) bool {
	switch name {
	case informationSchemaName:
	case pgCatalogName:
	case sqlbase.SystemDB.Name:
	case crdbInternalName:
	default:
		return false
	}
	return true
}

// forEachTableDescWithTableLookup acts like forEachTableDesc, except it also provides a
// tableLookupFn when calling fn to allow callers to lookup fetched table descriptors
// on demand. This is important for callers dealing with objects like foreign keys, where
// the metadata for each object must be augmented by looking at the referenced table.
//
// The prefix argument specifies in which database context we are
// requesting the descriptors.  In context "" all descriptors are
// visible, in non-empty contexts only the descriptors of that
// database are visible.
func forEachTableDescWithTableLookup(
	ctx context.Context,
	p *planner,
	prefix string,
	fn func(*sqlbase.DatabaseDescriptor, *sqlbase.TableDescriptor, tableLookupFn) error,
) error {
	return forEachTableDescWithTableLookupInternal(ctx, p, prefix, false /* allowAdding */, fn)
}

// forEachTableDescWithTableLookupInternal is the logic that supports
// forEachTableDescWithTableLookup.
//
// The allowAdding argument if true includes newly added tables that
// are not yet public.
func forEachTableDescWithTableLookupInternal(
	ctx context.Context,
	p *planner,
	prefix string,
	allowAdding bool,
	fn func(*sqlbase.DatabaseDescriptor, *sqlbase.TableDescriptor, tableLookupFn) error,
) error {
	type dbDescTables struct {
		desc       *sqlbase.DatabaseDescriptor
		tables     map[string]*sqlbase.TableDescriptor
		tablesByID map[sqlbase.ID]*sqlbase.TableDescriptor
	}
	databases := make(map[string]dbDescTables)

	// Handle real schemas.
	descs, err := p.Tables().getAllDescriptors(ctx, p.txn)
	if err != nil {
		return err
	}
	dbIDsToName := make(map[sqlbase.ID]string)
	// First, iterate through all database descriptors, constructing dbDescTables
	// objects and populating a mapping from sqlbase.ID to database name.
	for _, desc := range descs {
		if db, ok := desc.(*sqlbase.DatabaseDescriptor); ok {
			dbIDsToName[db.GetID()] = db.GetName()
			databases[db.GetName()] = dbDescTables{
				desc:       db,
				tables:     make(map[string]*sqlbase.TableDescriptor),
				tablesByID: make(map[sqlbase.ID]*sqlbase.TableDescriptor),
			}
		}
	}
	// Next, iterate through all table descriptors, using the mapping from sqlbase.ID
	// to database name to add descriptors to a dbDescTables' tables map.
	for _, desc := range descs {
		if table, ok := desc.(*sqlbase.TableDescriptor); ok && !table.Dropped() {
			dbName, ok := dbIDsToName[table.GetParentID()]
			if !ok {
				// Contrary to `crdb_internal.tables`, which for debugging
				// purposes also displays dropped tables which miss a parent
				// database (because the parent descriptor has already been
				// deleted), information_schema.tables is specified to only
				// report usable tables, so we exclude dropped tables and the
				// parent database must always exist.
				return errors.Errorf("no database with ID %d found", table.GetParentID())
			}
			dbTables := databases[dbName]
			dbTables.tables[table.Name] = table
			dbTables.tablesByID[table.ID] = table
		}
	}

	// Handle virtual schemas.
	for dbName, schema := range p.getVirtualTabler().getEntries() {
		dbTables := make(map[string]*sqlbase.TableDescriptor, len(schema.tables))
		for tableName, entry := range schema.tables {
			dbTables[tableName] = entry.desc
		}
		databases[dbName] = dbDescTables{
			desc:   schema.desc,
			tables: dbTables,
		}
	}

	// Create table lookup function, which some callers of this function, like those
	// dealing with foreign keys, will need.
	tableLookup := func(id sqlbase.ID) (*sqlbase.DatabaseDescriptor, *sqlbase.TableDescriptor) {
		for _, db := range databases {
			table, ok := db.tablesByID[id]
			if ok {
				return db.desc, table
			}
		}
		return nil, nil
	}

	// Below we use the same trick twice of sorting a slice of strings lexicographically
	// and iterating through these strings to index into a map. Effectively, this allows
	// us to iterate through a map in sorted order.
	dbNames := make([]string, 0, len(databases))
	for dbName := range databases {
		dbNames = append(dbNames, dbName)
	}
	sort.Strings(dbNames)
	for _, dbName := range dbNames {
		if !isDatabaseVisible(dbName, prefix, p.SessionData().User) {
			continue
		}
		db := databases[dbName]
		dbTableNames := make([]string, 0, len(db.tables))
		for tableName := range db.tables {
			dbTableNames = append(dbTableNames, tableName)
		}
		sort.Strings(dbTableNames)
		for _, tableName := range dbTableNames {
			tableDesc := db.tables[tableName]
			if userCanSeeTable(ctx, p, tableDesc, allowAdding) {
				if err := fn(db.desc, tableDesc, tableLookup); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func forEachIndexInTable(
	table *sqlbase.TableDescriptor, fn func(*sqlbase.IndexDescriptor) error,
) error {
	if table.IsPhysicalTable() {
		if err := fn(&table.PrimaryIndex); err != nil {
			return err
		}
	}
	for i := range table.Indexes {
		if err := fn(&table.Indexes[i]); err != nil {
			return err
		}
	}
	return nil
}

func forEachColumnInTable(
	table *sqlbase.TableDescriptor, fn func(*sqlbase.ColumnDescriptor) error,
) error {
	// Table descriptors already hold columns in-order.
	for i := range table.Columns {
		if !table.Columns[i].Hidden {
			if err := fn(&table.Columns[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

func forEachColumnInIndex(
	table *sqlbase.TableDescriptor,
	index *sqlbase.IndexDescriptor,
	fn func(*sqlbase.ColumnDescriptor) error,
) error {
	colMap := make(map[sqlbase.ColumnID]*sqlbase.ColumnDescriptor, len(table.Columns))
	for i, column := range table.Columns {
		colMap[column.ID] = &table.Columns[i]
	}
	for _, columnID := range index.ColumnIDs {
		if column := colMap[columnID]; !column.Hidden {
			if err := fn(column); err != nil {
				return err
			}
		}
	}
	return nil
}

func forEachRole(
	ctx context.Context, origPlanner *planner, fn func(username string, isRole bool) error,
) error {
	query := `SELECT username, "isRole" FROM system.users`
	p, cleanup := newInternalPlanner(
		"for-each-role", origPlanner.txn, security.RootUser,
		origPlanner.extendedEvalCtx.MemMetrics, origPlanner.ExecCfg(),
	)
	defer cleanup()
	rows, _ /* cols */, err := p.queryRows(ctx, query)
	if err != nil {
		return err
	}

	for _, row := range rows {
		username := tree.MustBeDString(row[0])
		isRole, ok := row[1].(*tree.DBool)
		if !ok {
			return errors.Errorf("isRole should be a boolean value, found %s instead", row[1].ResolvedType())
		}

		if err := fn(string(username), bool(*isRole)); err != nil {
			return err
		}
	}
	return nil
}

func forEachRoleMembership(
	ctx context.Context, origPlanner *planner, fn func(role, member string, isAdmin bool) error,
) error {
	query := `SELECT "role", "member", "isAdmin" FROM system.role_members`
	p, cleanup := newInternalPlanner(
		"for-each-role-member", origPlanner.txn, security.RootUser,
		origPlanner.extendedEvalCtx.MemMetrics, origPlanner.ExecCfg(),
	)
	defer cleanup()
	rows, _ /* cols */, err := p.queryRows(ctx, query)
	if err != nil {
		return err
	}

	for _, row := range rows {
		roleName := tree.MustBeDString(row[0])
		memberName := tree.MustBeDString(row[1])
		isAdmin := row[2].(*tree.DBool)

		if err := fn(string(roleName), string(memberName), bool(*isAdmin)); err != nil {
			return err
		}
	}
	return nil
}

func userCanSeeDatabase(ctx context.Context, p *planner, db *sqlbase.DatabaseDescriptor) bool {
	return p.CheckAnyPrivilege(ctx, db) == nil
}

func userCanSeeTable(
	ctx context.Context, p *planner, table *sqlbase.TableDescriptor, allowAdding bool,
) bool {
	if !(table.State == sqlbase.TableDescriptor_PUBLIC ||
		(allowAdding && table.State == sqlbase.TableDescriptor_ADD)) {
		return false
	}
	return p.CheckAnyPrivilege(ctx, table) == nil
}
