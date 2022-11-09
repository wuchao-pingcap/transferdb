/*
Copyright © 2020 Marvin

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package o2m

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/wentaojin/transferdb/config"
	"github.com/wentaojin/transferdb/utils"

	"github.com/wentaojin/transferdb/service"

	"github.com/valyala/fastjson"
	"go.uber.org/zap"
)

// 任务
type Table struct {
	SourceSchemaName      string
	TargetSchemaName      string
	SourceTableName       string
	TargetDBType          string
	TargetTableName       string
	TargetTableOption     string
	OracleCollation       bool
	SourceSchemaCollation string // 可为空
	SourceTableCollation  string // 可为空
	SourceDBNLSSort       string
	SourceDBNLSComp       string
	SourceTableType       string
	Overwrite             bool
	Engine                *service.Engine `json:"-"`
}

func (t Table) GenCreateTableSQL(modifyTableName string) (string, []string, error) {
	var (
		tableMetas     []string
		createTableSQL string
		tableCollation string
		compIndexINFO  []string
	)

	// schema、db、table collation
	if t.OracleCollation {
		// table collation
		if t.SourceTableCollation != "" {
			if val, ok := utils.OracleCollationMap[t.SourceTableCollation]; ok {
				tableCollation = val
			} else {
				return "", compIndexINFO, fmt.Errorf("oracle table collation [%v] isn't support", t.SourceTableCollation)
			}
		}
		// schema collation
		if t.SourceTableCollation == "" && t.SourceSchemaCollation != "" {
			if val, ok := utils.OracleCollationMap[t.SourceSchemaCollation]; ok {
				tableCollation = val
			} else {
				return "", compIndexINFO, fmt.Errorf("oracle schema collation [%v] table collation [%v] isn't support", t.SourceSchemaCollation, t.SourceTableCollation)
			}
		}
		if t.SourceTableName == "" && t.SourceSchemaCollation == "" {
			return "", compIndexINFO, fmt.Errorf("oracle schema collation [%v] table collation [%v] isn't support", t.SourceSchemaCollation, t.SourceTableCollation)
		}
	} else {
		// db collation
		if val, ok := utils.OracleCollationMap[t.SourceDBNLSComp]; ok {
			tableCollation = val
		} else {
			return "", compIndexINFO, fmt.Errorf("oracle db nls_comp [%v] nls_sort [%v] isn't support", t.SourceDBNLSComp, t.SourceDBNLSSort)
		}
	}

	// 唯一约束/普通索引/唯一索引
	ukINFO, err := t.GenCreateUKSQL()
	if err != nil {
		return "", compIndexINFO, fmt.Errorf("oracle db reverse table [%s] unique constraint failed: %v", modifyTableName, err)
	}

	indexINFO, compNonUniqueIndex, err := t.GenCreateNonUniqueIndex(modifyTableName)
	if err != nil {
		return "", compIndexINFO, fmt.Errorf("oracle db reverse table [%s] key non-unique index failed: %v", modifyTableName, err)
	}

	uniqueIndexINFO, compUniqueIndex, err := t.GenCreateUniqueIndex(modifyTableName)
	if err != nil {
		return "", compIndexINFO, fmt.Errorf("oracle db reverse table [%s] key unique index failed: %v", modifyTableName, err)
	}

	if len(compNonUniqueIndex) > 0 {
		compIndexINFO = append(compIndexINFO, compNonUniqueIndex...)
	}
	if len(compUniqueIndex) > 0 {
		compIndexINFO = append(compIndexINFO, compUniqueIndex...)
	}

	// tables 表结构
	pkMeta, pkINFO, err := t.reverserOracleTablePKToMySQL()
	if err != nil {
		return createTableSQL, compIndexINFO, err
	}

	columnMetaSlice, singlePKColumnTypeIsInteger, err := t.reverserOracleTableColumnToMySQL(t.OracleCollation, pkINFO)
	if err != nil {
		return createTableSQL, compIndexINFO, err
	}

	tablesMap, err := t.Engine.GetOracleTableComment(t.SourceSchemaName, t.SourceTableName)
	if err != nil {
		return createTableSQL, compIndexINFO, err
	}

	// table 表注释
	tableComment := tablesMap[0]["COMMENTS"]

	tableMetas = append(tableMetas, columnMetaSlice...)
	if len(pkMeta) > 0 {
		tableMetas = append(tableMetas, pkMeta...)
	}

	if len(ukINFO) > 0 {
		tableMetas = append(tableMetas, ukINFO...)
	}

	if len(indexINFO) > 0 {
		tableMetas = append(tableMetas, indexINFO...)
	}

	if len(uniqueIndexINFO) > 0 {
		tableMetas = append(tableMetas, uniqueIndexINFO...)
	}

	tableMeta := strings.Join(tableMetas, ",\n")

	// table-option 表后缀可选项
	if t.TargetDBType == utils.MySQLTargetDBType || t.TargetTableOption == "" {
		zap.L().Warn("reverse oracle table struct",
			zap.String("schema", t.SourceSchemaName),
			zap.String("table", t.SourceTableName),
			zap.String("comment", tablesMap[0]["COMMENTS"]),
			zap.Strings("columns", columnMetaSlice),
			zap.Strings("pk", pkMeta),
			zap.String("table-option", "table-option is null, would be disabled"))
		// table struct
		if tableComment != "" {
			createTableSQL = fmt.Sprintf("CREATE TABLE `%s`.`%s` (\n%s\n) ENGINE=InnoDB DEFAULT CHARSET=%s COLLATE=%s COMMENT='%s';",
				t.TargetSchemaName, modifyTableName, tableMeta, strings.ToLower(utils.MySQLCharacterSet), tableCollation, tableComment)
		} else {
			createTableSQL = fmt.Sprintf("CREATE TABLE `%s`.`%s` (\n%s\n) ENGINE=InnoDB DEFAULT CHARSET=%s COLLATE=%s;",
				t.TargetSchemaName, modifyTableName, tableMeta, strings.ToLower(utils.MySQLCharacterSet), tableCollation)
		}
	} else {
		// TiDB
		clusteredIdxVal, err := t.Engine.GetTiDBClusteredIndexValue()
		if err != nil {
			return createTableSQL, compIndexINFO, err
		}
		switch strings.ToUpper(clusteredIdxVal) {
		case utils.TiDBClusteredIndexOFFValue:
			zap.L().Warn("reverse oracle table struct",
				zap.String("schema", t.SourceSchemaName),
				zap.String("table", t.SourceTableName),
				zap.String("comment", tablesMap[0]["COMMENTS"]),
				zap.Strings("columns", columnMetaSlice),
				zap.Strings("pk", pkMeta),
				zap.String("tidb_enable_clustered_index", utils.TiDBClusteredIndexOFFValue),
				zap.String("table-option", "tidb_enable_clustered_index is off, would be enabled"))

			if tableComment != "" && t.TargetTableOption != "" {
				createTableSQL = fmt.Sprintf("CREATE TABLE `%s`.`%s` (\n%s\n) ENGINE=InnoDB DEFAULT CHARSET=%s COLLATE=%s %s COMMENT='%s';",
					t.TargetSchemaName, modifyTableName, tableMeta, strings.ToLower(utils.MySQLCharacterSet), tableCollation, strings.ToUpper(t.TargetTableOption), tableComment)
			} else if tableComment == "" && t.TargetTableOption != "" {
				createTableSQL = fmt.Sprintf("CREATE TABLE `%s`.`%s` (\n%s\n) ENGINE=InnoDB DEFAULT CHARSET=%s COLLATE=%s %s;",
					t.TargetSchemaName, modifyTableName, tableMeta, strings.ToLower(utils.MySQLCharacterSet), tableCollation, strings.ToUpper(t.TargetTableOption))
			} else if tableComment == "" && t.TargetTableOption == "" {
				createTableSQL = fmt.Sprintf("CREATE TABLE `%s`.`%s` (\n%s\n) ENGINE=InnoDB DEFAULT CHARSET=%s COLLATE=%s;",
					t.TargetSchemaName, modifyTableName, tableMeta, strings.ToLower(utils.MySQLCharacterSet), tableCollation)
			} else {
				createTableSQL = fmt.Sprintf("CREATE TABLE `%s`.`%s` (\n%s\n) ENGINE=InnoDB DEFAULT CHARSET=%s COLLATE=%s COMMENT='%s';",
					t.TargetSchemaName, modifyTableName, tableMeta, strings.ToLower(utils.MySQLCharacterSet), tableCollation, tableComment)
			}
		case utils.TiDBClusteredIndexONValue:
			zap.L().Warn("reverse oracle table struct",
				zap.String("schema", t.SourceSchemaName),
				zap.String("table", t.SourceTableName),
				zap.String("comment", tablesMap[0]["COMMENTS"]),
				zap.Strings("columns", columnMetaSlice),
				zap.Strings("pk", pkMeta),
				zap.String("tidb_enable_clustered_index", utils.TiDBClusteredIndexONValue),
				zap.String("table-option", "tidb_enable_clustered_index is on, would be disabled"))

			if tableComment != "" {
				createTableSQL = fmt.Sprintf("CREATE TABLE `%s`.`%s` (\n%s\n) ENGINE=InnoDB DEFAULT CHARSET=%s COLLATE=%s COMMENT='%s';",
					t.TargetSchemaName, modifyTableName, tableMeta, strings.ToLower(utils.MySQLCharacterSet), tableCollation, tableComment)
			} else {
				createTableSQL = fmt.Sprintf("CREATE TABLE `%s`.`%s` (\n%s\n) ENGINE=InnoDB DEFAULT CHARSET=%s COLLATE=%s;",
					t.TargetSchemaName, modifyTableName, tableMeta, strings.ToLower(utils.MySQLCharacterSet), tableCollation)
			}
		default:
			// tidb_enable_clustered_index = int_only / tidb_enable_clustered_index 不存在值，等于空
			pkVal, err := t.Engine.GetTiDBAlterPKValue()
			if err != nil {
				return createTableSQL, compIndexINFO, err
			}
			if !fastjson.Exists([]byte(pkVal), "alter-primary-key") {
				zap.L().Warn("reverse oracle table struct",
					zap.String("schema", t.SourceSchemaName),
					zap.String("table", t.SourceTableName),
					zap.String("comment", tablesMap[0]["COMMENTS"]),
					zap.Strings("columns", columnMetaSlice),
					zap.Strings("pk", pkMeta),
					zap.String("tidb_enable_clustered_index", strings.ToUpper(clusteredIdxVal)),
					zap.String("alter-primary-key", "not exist"),
					zap.String("table-option", "alter-primary-key isn't exits, would be disable"))
				if tableComment != "" {
					createTableSQL = fmt.Sprintf("CREATE TABLE `%s`.`%s` (\n%s\n) ENGINE=InnoDB DEFAULT CHARSET=%s COLLATE=%s COMMENT='%s';",
						t.TargetSchemaName, modifyTableName, tableMeta, strings.ToLower(utils.MySQLCharacterSet), tableCollation, tableComment)
				} else {
					createTableSQL = fmt.Sprintf("CREATE TABLE `%s`.`%s` (\n%s\n) ENGINE=InnoDB DEFAULT CHARSET=%s COLLATE=%s;",
						t.TargetSchemaName, modifyTableName, tableMeta, strings.ToLower(utils.MySQLCharacterSet), tableCollation)
				}
			} else {
				var p fastjson.Parser
				v, err := p.Parse(pkVal)
				if err != nil {
					return createTableSQL, compIndexINFO, err
				}

				isAlterPK := v.GetBool("alter-primary-key")

				// alter-primary-key = false
				// 整型主键 table-option 不生效
				// 单列主键是整型
				if !isAlterPK && len(pkINFO) == 1 && singlePKColumnTypeIsInteger {
					zap.L().Warn("reverse oracle table struct",
						zap.String("schema", t.SourceSchemaName),
						zap.String("table", t.SourceTableName),
						zap.String("comment", tablesMap[0]["COMMENTS"]),
						zap.Strings("columns", columnMetaSlice),
						zap.Strings("pk", pkMeta),
						zap.String("tidb_enable_clustered_index", strings.ToUpper(clusteredIdxVal)),
						zap.Bool("alter-primary-key", isAlterPK),
						zap.String("table-option", "integer primary key, would be disable"))

					if tableComment != "" {
						createTableSQL = fmt.Sprintf("CREATE TABLE `%s`.`%s` (\n%s\n) ENGINE=InnoDB DEFAULT CHARSET=%s COLLATE=%s COMMENT='%s';",
							t.TargetSchemaName, modifyTableName, tableMeta, strings.ToLower(utils.MySQLCharacterSet), tableCollation, tableComment)
					} else {
						createTableSQL = fmt.Sprintf("CREATE TABLE `%s`.`%s` (\n%s\n) ENGINE=InnoDB DEFAULT CHARSET=%s COLLATE=%s;",
							t.TargetSchemaName, modifyTableName, tableMeta, strings.ToLower(utils.MySQLCharacterSet), tableCollation)
					}
				} else {
					// table-option 生效
					// alter-primary-key = true
					// alter-primary-key = false && 联合主键 len(pkINFO)>1
					// alter-primary-key = false && 非整型主键
					if isAlterPK || (!isAlterPK && len(pkINFO) > 1) || (!isAlterPK && !singlePKColumnTypeIsInteger) {
						zap.L().Warn("reverse oracle table struct",
							zap.String("schema", t.SourceSchemaName),
							zap.String("table", t.SourceTableName),
							zap.String("comment", tablesMap[0]["COMMENTS"]),
							zap.Strings("columns", columnMetaSlice),
							zap.Strings("pk", pkMeta),
							zap.String("tidb_enable_clustered_index", strings.ToUpper(clusteredIdxVal)),
							zap.Bool("alter-primary-key", isAlterPK),
							zap.String("table-option", "enabled"))
						if tableComment != "" && t.TargetTableOption != "" {
							createTableSQL = fmt.Sprintf("CREATE TABLE `%s`.`%s` (\n%s\n) ENGINE=InnoDB DEFAULT CHARSET=%s COLLATE=%s %s COMMENT='%s';",
								t.TargetSchemaName, modifyTableName, tableMeta, strings.ToLower(utils.MySQLCharacterSet), tableCollation, strings.ToUpper(t.TargetTableOption), tableComment)
						} else if tableComment == "" && t.TargetTableOption != "" {
							createTableSQL = fmt.Sprintf("CREATE TABLE `%s`.`%s` (\n%s\n) ENGINE=InnoDB DEFAULT CHARSET=%s COLLATE=%s %s;",
								t.TargetSchemaName, modifyTableName, tableMeta, strings.ToLower(utils.MySQLCharacterSet), tableCollation, strings.ToUpper(t.TargetTableOption))
						} else if tableComment == "" && t.TargetTableOption == "" {
							createTableSQL = fmt.Sprintf("CREATE TABLE `%s`.`%s` (\n%s\n) ENGINE=InnoDB DEFAULT CHARSET=%s COLLATE=%s;",
								t.TargetSchemaName, modifyTableName, tableMeta, strings.ToLower(utils.MySQLCharacterSet), tableCollation)
						} else {
							createTableSQL = fmt.Sprintf("CREATE TABLE `%s`.`%s` (\n%s\n) ENGINE=InnoDB DEFAULT CHARSET=%s COLLATE=%s COMMENT='%s';",
								t.TargetSchemaName, modifyTableName, tableMeta, strings.ToLower(utils.MySQLCharacterSet), tableCollation, tableComment)
						}
					} else {
						zap.L().Error("reverse oracle table struct",
							zap.String("schema", t.SourceSchemaName),
							zap.String("table", t.SourceTableName),
							zap.String("comment", tablesMap[0]["COMMENTS"]),
							zap.Strings("columns", columnMetaSlice),
							zap.Strings("pk", pkMeta),
							zap.String("tidb_enable_clustered_index", strings.ToUpper(clusteredIdxVal)),
							zap.Bool("alter-primary-key", isAlterPK),
							zap.String("table-option", "disabled"),
							zap.Error(fmt.Errorf("not support")))
						return createTableSQL, compIndexINFO, fmt.Errorf("reverse oracle table struct error: table-option not support")
					}
				}
			}
		}
	}

	zap.L().Info("reverse oracle table struct",
		zap.String("schema", t.SourceSchemaName),
		zap.String("table", t.SourceTableName),
		zap.String("comment", tablesMap[0]["COMMENTS"]),
		zap.Strings("columns", columnMetaSlice),
		zap.Strings("pk", pkMeta),
		zap.String("create sql", createTableSQL))

	return createTableSQL, compIndexINFO, nil
}

func (t Table) GenCreateUKSQL() ([]string, error) {
	var ukArr []string
	ukMetas, err := t.reverserOracleTableUKToMySQL()
	if err != nil {
		return ukArr, err
	}
	return ukMetas, nil
}

func (t Table) GenCreateFKSQL(modifyTableName string) ([]string, error) {
	var fkArr []string
	fkMetas, err := t.reverserOracleTableFKToMySQL()
	if err != nil {
		return fkArr, err
	}

	if len(fkMetas) > 0 {
		for _, fk := range fkMetas {
			addFkSQL := fmt.Sprintf("ALTER TABLE `%s`.`%s` ADD %s;", t.TargetSchemaName, modifyTableName, fk)
			zap.L().Info("reverse",
				zap.String("schema", t.TargetSchemaName),
				zap.String("table", modifyTableName),
				zap.String("fk sql", addFkSQL))

			fkArr = append(fkArr, addFkSQL)
		}
	}
	return fkArr, nil
}

func (t Table) GenCreateCKSQL(modifyTableName string) ([]string, error) {
	var ckArr []string
	ckMetas, err := t.reverserOracleTableCKToMySQL()
	if err != nil {
		return ckArr, err
	}

	if len(ckMetas) > 0 {
		for _, ck := range ckMetas {
			ckSQL := fmt.Sprintf("ALTER TABLE `%s`.`%s` ADD %s;", t.TargetSchemaName, modifyTableName, ck)
			zap.L().Info("reverse",
				zap.String("schema", t.TargetSchemaName),
				zap.String("table", modifyTableName),
				zap.String("ck sql", ckSQL))

			ckArr = append(ckArr, ckSQL)
		}
	}

	return ckArr, nil
}

func (t Table) GenCreateNonUniqueIndex(modifyTableName string) ([]string, []string, error) {
	// 普通索引【普通索引、函数索引、位图索引】
	return t.reverserOracleTableNormalIndexToMySQL(modifyTableName)
}

func (t Table) GenCreateUniqueIndex(modifyTableName string) ([]string, []string, error) {
	// 唯一索引
	return t.reverserOracleTableUniqueIndexToMySQL(modifyTableName)
}

func (t Table) reverserOracleTableNormalIndexToMySQL(modifyTableName string) ([]string, []string, error) {
	var (
		keyINFO               []string
		compatibilityIndexSQL []string
	)
	indexesMap, err := t.Engine.GetOracleTableNormalIndex(t.SourceSchemaName, t.SourceTableName)
	if err != nil {
		return keyINFO, compatibilityIndexSQL, err
	}
	if len(indexesMap) > 0 {
		for _, idxMeta := range indexesMap {
			if idxMeta["TABLE_NAME"] != "" && strings.ToUpper(idxMeta["UNIQUENESS"]) == "NONUNIQUE" {
				switch idxMeta["INDEX_TYPE"] {
				case "NORMAL":
					var normalIndex []string
					for _, col := range strings.Split(idxMeta["COLUMN_LIST"], ",") {
						normalIndex = append(normalIndex, fmt.Sprintf("`%s`", col))
					}

					keyIndex := fmt.Sprintf("KEY `%s` (%s)", strings.ToUpper(idxMeta["INDEX_NAME"]), strings.Join(normalIndex, ","))

					keyINFO = append(keyINFO, keyIndex)

					zap.L().Info("reverse normal index",
						zap.String("schema", t.SourceSchemaName),
						zap.String("table", idxMeta["TABLE_NAME"]),
						zap.String("index name", idxMeta["INDEX_NAME"]),
						zap.String("index type", idxMeta["INDEX_TYPE"]),
						zap.String("index column list", idxMeta["COLUMN_LIST"]),
						zap.String("key index info", keyIndex))

					continue

				case "FUNCTION-BASED NORMAL":
					sql := fmt.Sprintf("CREATE INDEX %s ON %s.%s (%s);",
						strings.ToUpper(idxMeta["INDEX_NAME"]), t.TargetSchemaName, modifyTableName,
						idxMeta["COLUMN_LIST"])

					compatibilityIndexSQL = append(compatibilityIndexSQL, sql)

					zap.L().Warn("reverse normal index",
						zap.String("schema", t.SourceSchemaName),
						zap.String("table", idxMeta["TABLE_NAME"]),
						zap.String("index name", idxMeta["INDEX_NAME"]),
						zap.String("index type", idxMeta["INDEX_TYPE"]),
						zap.String("index column list", idxMeta["COLUMN_LIST"]),
						zap.String("create normal index sql", sql),
						zap.String("warn", "mysql not support"))
					continue

				case "BITMAP":
					sql := fmt.Sprintf("CREATE BITMAP INDEX %s ON %s.%s (%s);",
						strings.ToUpper(idxMeta["INDEX_NAME"]), t.TargetSchemaName, modifyTableName,
						idxMeta["COLUMN_LIST"])

					compatibilityIndexSQL = append(compatibilityIndexSQL, sql)

					zap.L().Warn("reverse normal index",
						zap.String("schema", t.SourceSchemaName),
						zap.String("table", idxMeta["TABLE_NAME"]),
						zap.String("index name", idxMeta["INDEX_NAME"]),
						zap.String("index type", idxMeta["INDEX_TYPE"]),
						zap.String("index column list", idxMeta["COLUMN_LIST"]),
						zap.String("create normal index sql", sql),
						zap.String("warn", "mysql not support"))
					continue

				case "FUNCTION-BASED BITMAP":
					sql := fmt.Sprintf("CREATE BITMAP INDEX %s ON %s.%s (%s);",
						strings.ToUpper(idxMeta["INDEX_NAME"]), t.TargetSchemaName, modifyTableName,
						idxMeta["COLUMN_LIST"])

					compatibilityIndexSQL = append(compatibilityIndexSQL, sql)

					zap.L().Warn("reverse normal index",
						zap.String("schema", t.SourceSchemaName),
						zap.String("table", idxMeta["TABLE_NAME"]),
						zap.String("index name", idxMeta["INDEX_NAME"]),
						zap.String("index type", idxMeta["INDEX_TYPE"]),
						zap.String("index column list", idxMeta["COLUMN_LIST"]),
						zap.String("create normal index sql", sql),
						zap.String("warn", "mysql not support"))
					continue

				case "DOMAIN":
					sql := fmt.Sprintf("CREATE INDEX %s ON %s.%s (%s) INDEXTYPE IS %s.%s PARAMETERS ('%s');",
						strings.ToUpper(idxMeta["INDEX_NAME"]), t.TargetSchemaName, modifyTableName,
						idxMeta["COLUMN_LIST"],
						strings.ToUpper(idxMeta["ITYP_OWNER"]),
						strings.ToUpper(idxMeta["ITYP_NAME"]),
						idxMeta["PARAMETERS"])

					compatibilityIndexSQL = append(compatibilityIndexSQL, sql)

					zap.L().Warn("reverse normal index",
						zap.String("schema", t.SourceSchemaName),
						zap.String("table", idxMeta["TABLE_NAME"]),
						zap.String("index name", idxMeta["INDEX_NAME"]),
						zap.String("index type", idxMeta["INDEX_TYPE"]),
						zap.String("index column list", idxMeta["COLUMN_LIST"]),
						zap.String("domain owner", idxMeta["ITYP_OWNER"]),
						zap.String("domain index name", idxMeta["ITYP_NAME"]),
						zap.String("domain parameters", idxMeta["PARAMETERS"]),
						zap.String("create normal index sql", sql),
						zap.String("warn", "mysql not support"))
					continue

				default:
					zap.L().Error("reverse normal index",
						zap.String("schema", t.SourceSchemaName),
						zap.String("table", idxMeta["TABLE_NAME"]),
						zap.String("index name", idxMeta["INDEX_NAME"]),
						zap.String("index type", idxMeta["INDEX_TYPE"]),
						zap.String("index column list", idxMeta["COLUMN_LIST"]),
						zap.String("domain owner", idxMeta["ITYP_OWNER"]),
						zap.String("domain index name", idxMeta["ITYP_NAME"]),
						zap.String("domain parameters", idxMeta["PARAMETERS"]),
						zap.String("error", "mysql not support"))

					return keyINFO, compatibilityIndexSQL, fmt.Errorf("[NORMAL] oracle schema [%s] table [%s] reverse normal index panic, error: %v", t.SourceSchemaName, t.SourceTableName, idxMeta)
				}
			}

			zap.L().Error("reverse normal index",
				zap.String("schema", t.SourceSchemaName),
				zap.String("table", idxMeta["TABLE_NAME"]),
				zap.String("index name", idxMeta["INDEX_NAME"]),
				zap.String("index type", idxMeta["INDEX_TYPE"]),
				zap.String("index column list", idxMeta["COLUMN_LIST"]),
				zap.String("domain owner", idxMeta["ITYP_OWNER"]),
				zap.String("domain index name", idxMeta["ITYP_NAME"]),
				zap.String("domain parameters", idxMeta["PARAMETERS"]))
			return keyINFO, compatibilityIndexSQL, fmt.Errorf("[NON-NORMAL] oracle schema [%s] table [%s] reverse normal index panic, error: %v", t.SourceSchemaName, t.SourceTableName, idxMeta)
		}
	}

	return keyINFO, compatibilityIndexSQL, err
}

func (t Table) reverserOracleTableUniqueIndexToMySQL(modifyTableName string) ([]string, []string, error) {
	var (
		uniqueINFO            []string
		compatibilityIndexSQL []string
	)

	indexesMap, err := t.Engine.GetOracleTableUniqueIndex(t.SourceSchemaName, t.SourceTableName)
	if err != nil {
		return uniqueINFO, compatibilityIndexSQL, err
	}

	if len(indexesMap) > 0 {
		for _, idxMeta := range indexesMap {
			if idxMeta["TABLE_NAME"] != "" && strings.ToUpper(idxMeta["UNIQUENESS"]) == "UNIQUE" {
				switch idxMeta["INDEX_TYPE"] {
				case "NORMAL":
					var uniqueIndex []string
					for _, col := range strings.Split(idxMeta["COLUMN_LIST"], ",") {
						uniqueIndex = append(uniqueIndex, fmt.Sprintf("`%s`", col))
					}

					uniqueIDX := fmt.Sprintf("UNIQUE INDEX `%s` (%s)", strings.ToUpper(idxMeta["INDEX_NAME"]), strings.Join(uniqueIndex, ","))

					uniqueINFO = append(uniqueINFO, uniqueIDX)

					zap.L().Info("reverse unique index",
						zap.String("schema", t.SourceSchemaName),
						zap.String("table", idxMeta["TABLE_NAME"]),
						zap.String("index name", idxMeta["INDEX_NAME"]),
						zap.String("index type", idxMeta["INDEX_TYPE"]),
						zap.String("index column list", idxMeta["COLUMN_LIST"]),
						zap.String("unique index info", uniqueIDX))

					continue

				case "FUNCTION-BASED NORMAL":
					sql := fmt.Sprintf("CREATE UNIQUE INDEX `%s` ON `%s`.`%s` (%s);",
						strings.ToUpper(idxMeta["INDEX_NAME"]), t.TargetSchemaName, modifyTableName,
						idxMeta["COLUMN_LIST"])

					compatibilityIndexSQL = append(compatibilityIndexSQL, sql)

					zap.L().Warn("reverse unique key",
						zap.String("schema", t.SourceSchemaName),
						zap.String("table", idxMeta["TABLE_NAME"]),
						zap.String("index name", idxMeta["INDEX_NAME"]),
						zap.String("index type", idxMeta["INDEX_TYPE"]),
						zap.String("index column list", idxMeta["COLUMN_LIST"]),
						zap.String("create unique index sql", sql),
						zap.String("warn", "mysql not support"))

					continue

				default:
					zap.L().Error("reverse unique index",
						zap.String("schema", t.SourceSchemaName),
						zap.String("table", idxMeta["TABLE_NAME"]),
						zap.String("index name", idxMeta["INDEX_NAME"]),
						zap.String("index type", idxMeta["INDEX_TYPE"]),
						zap.String("index column list", idxMeta["COLUMN_LIST"]),
						zap.String("error", "mysql not support"))

					return uniqueINFO, compatibilityIndexSQL, fmt.Errorf("[UNIQUE] oracle schema [%s] table [%s] reverse normal index panic, error: %v", t.SourceSchemaName, t.SourceTableName, idxMeta)
				}
			}
			zap.L().Error("reverse unique key",
				zap.String("schema", t.SourceSchemaName),
				zap.String("table", idxMeta["TABLE_NAME"]),
				zap.String("index name", idxMeta["INDEX_NAME"]),
				zap.String("index type", idxMeta["INDEX_TYPE"]),
				zap.String("index column list", idxMeta["COLUMN_LIST"]))
			return uniqueINFO, compatibilityIndexSQL,
				fmt.Errorf("[NON-UNIQUE] oracle schema [%s] table [%s] panic, error: %v", t.SourceSchemaName, t.SourceTableName, idxMeta)
		}
	}

	return uniqueINFO, compatibilityIndexSQL, err
}

func (t Table) reverserOracleTableColumnToMySQL(oraCollation bool, pkColumnName []string) ([]string, bool, error) {
	var (
		// 字段元数据组
		columnMetas []string
		// 单列主键是否整型类型
		singlePKColumnTypeIsInteger bool
	)
	// 单列主键非整型类型
	singlePKColumnTypeIsInteger = false

	// 获取表数据字段列信息
	columnsMap, err := t.Engine.GetOracleTableColumn(t.SourceSchemaName, t.SourceTableName, oraCollation)
	if err != nil {
		return columnMetas, singlePKColumnTypeIsInteger, err
	}

	// 联合主键判断
	if len(pkColumnName) == 1 {
		// 单列主键
		for _, rowCol := range columnsMap {
			var (
				columnMeta string
				err        error
			)
			if oraCollation {
				columnMeta, err = ReverseOracleTableColumnMapRule(
					t.SourceSchemaName,
					t.SourceTableName,
					rowCol["COLUMN_NAME"],
					rowCol["DATA_TYPE"],
					rowCol["NULLABLE"],
					rowCol["COMMENTS"],
					rowCol["DATA_DEFAULT"],
					rowCol["DATA_SCALE"],
					rowCol["DATA_PRECISION"],
					rowCol["DATA_LENGTH"],
					rowCol["COLLATION"],
					t.Engine,
				)
			} else {
				columnMeta, err = ReverseOracleTableColumnMapRule(
					t.SourceSchemaName,
					t.SourceTableName,
					rowCol["COLUMN_NAME"],
					rowCol["DATA_TYPE"],
					rowCol["NULLABLE"],
					rowCol["COMMENTS"],
					rowCol["DATA_DEFAULT"],
					rowCol["DATA_SCALE"],
					rowCol["DATA_PRECISION"],
					rowCol["DATA_LENGTH"],
					"",
					t.Engine,
				)
			}
			if err != nil {
				return columnMetas, singlePKColumnTypeIsInteger, err
			}

			columnMetas = append(columnMetas, columnMeta)

			// 单列主键数据类型获取判断
			if strings.ToUpper(pkColumnName[0]) == utils.StringsBuilder("`", strings.ToUpper(rowCol["COLUMN_NAME"]), "`") {
				// Map 规则转换后的字段对应数据类型
				// columnMeta 视角 columnName columnType ....
				columnType := strings.Fields(columnMeta)[1]
				for _, integerType := range utils.TiDBIntegerPrimaryKeyList {
					if find := strings.Contains(strings.ToUpper(columnType), strings.ToUpper(integerType)); find {
						singlePKColumnTypeIsInteger = true
					}
				}
			}
		}
	} else {
		// 联合主键
		// Oracle 表字段数据类型内置映射 MySQL/TiDB 规则转换
		for _, rowCol := range columnsMap {
			var (
				columnMeta string
				err        error
			)
			if oraCollation {
				columnMeta, err = ReverseOracleTableColumnMapRule(
					t.SourceSchemaName,
					t.SourceTableName,
					rowCol["COLUMN_NAME"],
					rowCol["DATA_TYPE"],
					rowCol["NULLABLE"],
					rowCol["COMMENTS"],
					rowCol["DATA_DEFAULT"],
					rowCol["DATA_SCALE"],
					rowCol["DATA_PRECISION"],
					rowCol["DATA_LENGTH"],
					rowCol["COLLATION"],
					t.Engine,
				)
			} else {
				columnMeta, err = ReverseOracleTableColumnMapRule(
					t.SourceSchemaName,
					t.SourceTableName,
					rowCol["COLUMN_NAME"],
					rowCol["DATA_TYPE"],
					rowCol["NULLABLE"],
					rowCol["COMMENTS"],
					rowCol["DATA_DEFAULT"],
					rowCol["DATA_SCALE"],
					rowCol["DATA_PRECISION"],
					rowCol["DATA_LENGTH"],
					"",
					t.Engine,
				)
			}
			if err != nil {
				return columnMetas, singlePKColumnTypeIsInteger, err
			}
			columnMetas = append(columnMetas, columnMeta)
		}
	}

	return columnMetas, singlePKColumnTypeIsInteger, nil
}

func (t Table) reverserOracleTablePKToMySQL() ([]string, []string, error) {
	var (
		keysMeta []string
		pkArr    []string
	)
	primaryKeyMap, err := t.Engine.GetOracleTablePrimaryKey(t.SourceSchemaName, t.SourceTableName)
	if err != nil {
		return keysMeta, pkArr, err
	}

	if len(primaryKeyMap) > 1 {
		return keysMeta, pkArr, fmt.Errorf("oracle schema [%s] table [%s] primary key exist multiple values: [%v]", t.SourceSchemaName, t.SourceTableName, primaryKeyMap)
	}
	if len(primaryKeyMap) > 0 {
		for _, col := range strings.Split(primaryKeyMap[0]["COLUMN_LIST"], ",") {
			pkArr = append(pkArr, fmt.Sprintf("`%s`", col))
		}
		pk := fmt.Sprintf("PRIMARY KEY (%s)", strings.ToUpper(strings.Join(pkArr, ",")))
		keysMeta = append(keysMeta, pk)
	}
	return keysMeta, pkArr, nil
}

func (t Table) reverserOracleTableUKToMySQL() ([]string, error) {
	var keysMeta []string
	uniqueKeyMap, err := t.Engine.GetOracleTableUniqueKey(t.SourceSchemaName, t.SourceTableName)
	if err != nil {
		return keysMeta, err
	}
	if len(uniqueKeyMap) > 0 {
		for _, rowUKCol := range uniqueKeyMap {
			var ukArr []string
			for _, col := range strings.Split(rowUKCol["COLUMN_LIST"], ",") {
				ukArr = append(ukArr, fmt.Sprintf("`%s`", col))
			}
			uk := fmt.Sprintf("UNIQUE KEY `%s` (%s)",
				strings.ToUpper(rowUKCol["CONSTRAINT_NAME"]), strings.ToUpper(strings.Join(ukArr, ",")))

			keysMeta = append(keysMeta, uk)
		}
	}
	return keysMeta, nil
}

func (t Table) reverserOracleTableFKToMySQL() ([]string, error) {
	var keysMeta []string
	foreignKeyMap, err := t.Engine.GetOracleTableForeignKey(t.SourceSchemaName, t.SourceTableName)
	if err != nil {
		return keysMeta, err
	}

	if len(foreignKeyMap) > 0 {
		for _, rowFKCol := range foreignKeyMap {
			if rowFKCol["DELETE_RULE"] == "" || rowFKCol["DELETE_RULE"] == "NO ACTION" {
				fk := fmt.Sprintf("CONSTRAINT `%s` FOREIGN KEY (%s) REFERENCES `%s`.`%s` (%s)",
					strings.ToUpper(rowFKCol["CONSTRAINT_NAME"]),
					strings.ToUpper(rowFKCol["COLUMN_LIST"]),
					strings.ToUpper(rowFKCol["R_OWNER"]),
					strings.ToUpper(rowFKCol["RTABLE_NAME"]),
					strings.ToUpper(rowFKCol["RCOLUMN_LIST"]))
				keysMeta = append(keysMeta, fk)
			}
			if rowFKCol["DELETE_RULE"] == "CASCADE" {
				fk := fmt.Sprintf("CONSTRAINT `%s` FOREIGN KEY (%s) REFERENCES `%s`.`%s`(%s) ON DELETE CASCADE",
					strings.ToUpper(rowFKCol["CONSTRAINT_NAME"]),
					strings.ToUpper(rowFKCol["COLUMN_LIST"]),
					strings.ToUpper(rowFKCol["R_OWNER"]),
					strings.ToUpper(rowFKCol["RTABLE_NAME"]),
					strings.ToUpper(rowFKCol["RCOLUMN_LIST"]))
				keysMeta = append(keysMeta, fk)
			}
			if rowFKCol["DELETE_RULE"] == "SET NULL" {
				fk := fmt.Sprintf("CONSTRAINT `%s` FOREIGN KEY(%s) REFERENCES `%s`.`%s`(%s) ON DELETE SET NULL",
					strings.ToUpper(rowFKCol["CONSTRAINT_NAME"]),
					strings.ToUpper(rowFKCol["COLUMN_LIST"]),
					strings.ToUpper(rowFKCol["R_OWNER"]),
					strings.ToUpper(rowFKCol["RTABLE_NAME"]),
					strings.ToUpper(rowFKCol["RCOLUMN_LIST"]))
				keysMeta = append(keysMeta, fk)
			}
		}
	}

	return keysMeta, nil
}

func (t Table) reverserOracleTableCKToMySQL() ([]string, error) {
	var keysMeta []string
	checkKeyMap, err := t.Engine.GetOracleTableCheckKey(t.SourceSchemaName, t.SourceTableName)
	if err != nil {
		return keysMeta, err
	}
	if len(checkKeyMap) > 0 {
		// 多个检查约束匹配
		// 比如："LOC" IS noT nUll and loc in ('a','b','c')
		r, err := regexp.Compile(`\s+(?i:AND)\s+|\s+(?i:OR)\s+`)
		if err != nil {
			return keysMeta, fmt.Errorf("check constraint regexp [AND/OR] failed: %v", err)
		}

		matchRex, err := regexp.Compile(`(^.*)(?i:IS NOT NULL)`)
		if err != nil {
			return keysMeta, fmt.Errorf("check constraint regexp match [IS NOT NULL] failed: %v", err)
		}

		checkRex, err := regexp.Compile(`(.*)(?i:IS NOT NULL)`)
		if err != nil {
			fmt.Printf("check constraint regexp check [IS NOT NULL] failed: %v", err)
		}

		for _, rowCKCol := range checkKeyMap {
			// 排除非空约束检查
			s := strings.TrimSpace(rowCKCol["SEARCH_CONDITION"])

			if !r.MatchString(s) {
				if !matchRex.MatchString(s) {
					keysMeta = append(keysMeta, fmt.Sprintf("CONSTRAINT `%s` CHECK (%s)",
						strings.ToUpper(rowCKCol["CONSTRAINT_NAME"]),
						rowCKCol["SEARCH_CONDITION"]))
				}
			} else {

				strArray := strings.Fields(s)

				var (
					idxArray        []int
					checkArray      []string
					constraintArray []string
				)
				for idx, val := range strArray {
					if strings.EqualFold(val, "AND") || strings.EqualFold(val, "OR") {
						idxArray = append(idxArray, idx)
					}
				}

				idxArray = append(idxArray, len(strArray))

				for idx, val := range idxArray {
					if idx == 0 {
						checkArray = append(checkArray, strings.Join(strArray[0:val], " "))
					} else {
						checkArray = append(checkArray, strings.Join(strArray[idxArray[idx-1]:val], " "))
					}
				}

				for _, val := range checkArray {
					v := strings.TrimSpace(val)
					if !checkRex.MatchString(v) {
						constraintArray = append(constraintArray, v)
					}
				}

				sd := strings.Join(constraintArray, " ")
				d := strings.Fields(sd)

				if strings.EqualFold(d[0], "AND") || strings.EqualFold(d[0], "OR") {
					d = d[1:]
				}
				if strings.EqualFold(d[len(d)-1], "AND") || strings.EqualFold(d[len(d)-1], "OR") {
					d = d[:len(d)-1]
				}

				keysMeta = append(keysMeta, fmt.Sprintf("CONSTRAINT `%s` CHECK (%s)",
					strings.ToUpper(rowCKCol["CONSTRAINT_NAME"]),
					strings.Join(d, " ")))
			}
		}
	}

	return keysMeta, nil
}

func (t *Table) String() string {
	jsonStr, _ := json.Marshal(t)
	return string(jsonStr)
}

// 表列表过滤 -> 不兼容性的
func GenOracleToMySQLTableList(engine *service.Engine, cfg *config.CfgFile, exporterTableSlice []string, nlsSort, nlsComp string) ([]Table, []string, []string, []string, error) {
	var tables []Table

	sourceSchema := strings.ToUpper(cfg.OracleConfig.SchemaName)

	beginTime := time.Now()
	defer func() {
		endTime := time.Now()
		zap.L().Info("gen oracle table list finished",
			zap.String("schema", sourceSchema),
			zap.Int("table totals", len(exporterTableSlice)),
			zap.Int("table gens", len(tables)),
			zap.String("cost", endTime.Sub(beginTime).String()))
	}()

	// 筛选过滤可能不支持的表类型
	partitionTables, err := engine.FilterOraclePartitionTable(sourceSchema, exporterTableSlice)
	if err != nil {
		return []Table{}, partitionTables, []string{}, []string{}, err
	}
	temporaryTables, err := engine.FilterOracleTemporaryTable(sourceSchema, exporterTableSlice)
	if err != nil {
		return []Table{}, []string{}, temporaryTables, []string{}, err
	}
	clusteredTables, err := engine.FilterOracleClusteredTable(sourceSchema, exporterTableSlice)
	if err != nil {
		return []Table{}, []string{}, []string{}, clusteredTables, err
	}

	if len(partitionTables) != 0 {
		zap.L().Warn("partition tables",
			zap.String("schema", sourceSchema),
			zap.String("partition table list", fmt.Sprintf("%v", partitionTables)),
			zap.String("suggest", "if necessary, please manually convert and process the tables in the above list"))
	}
	if len(temporaryTables) != 0 {
		zap.L().Warn("temporary tables",
			zap.String("schema", sourceSchema),
			zap.String("temporary table list", fmt.Sprintf("%v", temporaryTables)),
			zap.String("suggest", "if necessary, please manually process the tables in the above list"))
	}
	if len(clusteredTables) != 0 {
		zap.L().Warn("clustered tables",
			zap.String("schema", sourceSchema),
			zap.String("clustered table list", fmt.Sprintf("%v", clusteredTables)),
			zap.String("suggest", "if necessary, please manually process the tables in the above list"))
	}

	// oracle 环境信息
	startTime := time.Now()
	characterSet, err := engine.GetOracleDBCharacterSet()
	if err != nil {
		return []Table{}, partitionTables, temporaryTables, clusteredTables, err
	}
	if _, ok := utils.OracleDBCharacterSetMap[strings.Split(characterSet, ".")[1]]; !ok {
		return []Table{}, partitionTables, temporaryTables, clusteredTables, fmt.Errorf("oracle db character set [%v] isn't support", characterSet)
	}

	// oracle 版本是否可指定表、字段 collation
	// oracle db nls_sort/nls_comp 值需要相等，USING_NLS_COMP 值取 nls_comp
	oraDBVersion, err := engine.GetOracleDBVersion()
	if err != nil {
		return []Table{}, partitionTables, temporaryTables, clusteredTables, err
	}

	oraCollation := false
	if utils.VersionOrdinal(oraDBVersion) >= utils.VersionOrdinal(utils.OracleTableColumnCollationDBVersion) {
		oraCollation = true
	}

	endTime := time.Now()
	zap.L().Info("get oracle db character and version finished",
		zap.String("schema", sourceSchema),
		zap.String("db version", oraDBVersion),
		zap.String("db character", characterSet),
		zap.Int("table totals", len(exporterTableSlice)),
		zap.Bool("table collation", oraCollation),
		zap.String("cost", endTime.Sub(startTime).String()))

	var (
		tblCollation    map[string]string
		schemaCollation string
	)

	if oraCollation {
		startTime = time.Now()
		schemaCollation, err = engine.GetOracleSchemaCollation(sourceSchema)
		if err != nil {
			return []Table{}, partitionTables, temporaryTables, clusteredTables, err
		}
		tblCollation, err = engine.GetOracleTableCollation(sourceSchema, schemaCollation)
		if err != nil {
			return []Table{}, partitionTables, temporaryTables, clusteredTables, err
		}
		endTime = time.Now()
		zap.L().Info("get oracle schema and table collation finished",
			zap.String("schema", sourceSchema),
			zap.String("db version", oraDBVersion),
			zap.String("db character", characterSet),
			zap.Int("table totals", len(exporterTableSlice)),
			zap.Bool("table collation", oraCollation),
			zap.String("cost", endTime.Sub(startTime).String()))
	}

	startTime = time.Now()
	tablesMap, err := engine.GetOracleTableType(sourceSchema)
	if err != nil {
		return []Table{}, partitionTables, temporaryTables, clusteredTables, err
	}
	endTime = time.Now()
	zap.L().Info("get oracle table type finished",
		zap.String("schema", sourceSchema),
		zap.String("db version", oraDBVersion),
		zap.String("db character", characterSet),
		zap.Int("table totals", len(exporterTableSlice)),
		zap.Bool("table collation", oraCollation),
		zap.String("cost", endTime.Sub(startTime).String()))

	startTime = time.Now()
	wg := &sync.WaitGroup{}
	chS := make(chan string, utils.BufferSize)
	chT := make(chan Table, utils.BufferSize)

	c := make(chan struct{})

	// 数据 Append
	go func(done func()) {
		for tbl := range chT {
			tables = append(tables, tbl)
		}
		done()
	}(func() {
		c <- struct{}{}
	})

	// 数据 Product
	go func() {
		for _, t := range exporterTableSlice {
			chS <- t
		}
		close(chS)
	}()

	// 数据处理
	for c := 0; c < cfg.AppConfig.Threads; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ts := range chS {
				// 库名、表名规则
				tbl := Table{
					SourceSchemaName:  strings.ToUpper(sourceSchema),
					TargetSchemaName:  strings.ToUpper(cfg.MySQLConfig.SchemaName),
					SourceTableName:   strings.ToUpper(ts),
					TargetDBType:      strings.ToUpper(cfg.MySQLConfig.DBType),
					TargetTableName:   strings.ToUpper(ts),
					TargetTableOption: strings.ToUpper(cfg.MySQLConfig.TableOption),
					SourceTableType:   tablesMap[ts],
					SourceDBNLSSort:   nlsSort,
					SourceDBNLSComp:   nlsComp,
					Overwrite:         cfg.MySQLConfig.Overwrite,
					Engine:            engine,
				}
				tbl.OracleCollation = oraCollation
				if oraCollation {
					tbl.SourceSchemaCollation = schemaCollation
					tbl.SourceTableCollation = tblCollation[strings.ToUpper(ts)]
				}
				chT <- tbl
			}
		}()
	}

	wg.Wait()
	close(chT)
	<-c

	endTime = time.Now()
	zap.L().Info("gen oracle slice table finished",
		zap.String("schema", sourceSchema),
		zap.Int("table totals", len(exporterTableSlice)),
		zap.Int("table gens", len(tables)),
		zap.String("cost", endTime.Sub(startTime).String()))

	return tables, partitionTables, temporaryTables, clusteredTables, nil
}