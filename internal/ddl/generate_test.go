package ddl_test

import (
	"errors"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
)

func strLiteral(s string) *ddl.Literal { return &ddl.Literal{Raw: s, String: true} }

func TestGenerate(t *testing.T) {
	tests := []struct {
		name string
		op   ddl.Operation
		res  ddl.ResolvedOptions
		want string
	}{
		{
			name: "rebuild_index full options",
			op: ddl.RebuildIndex{
				Schema: "dbo", Table: "DISPATCH", Index: "IX_DISPATCH", DataCompression: "PAGE",
			},
			res: ddl.ResolvedOptions{
				Online: true, Resumable: true, WaitAtLowPriority: true,
				AbortAfterWait: "SELF", MaxDurationMinutes: 1,
				SortInTempDB: true, MaxDOP: intPtr(4),
			},
			want: "ALTER INDEX [IX_DISPATCH] ON [dbo].[DISPATCH] REBUILD WITH " +
				"(ONLINE = ON (WAIT_AT_LOW_PRIORITY (MAX_DURATION = 1 MINUTES, ABORT_AFTER_WAIT = SELF)), " +
				"RESUMABLE = ON, MAXDOP = 4, SORT_IN_TEMPDB = ON, DATA_COMPRESSION = PAGE);",
		},
		{
			name: "rebuild_index no options",
			op:   ddl.RebuildIndex{Schema: "dbo", Table: "T", Index: "IX"},
			res:  ddl.ResolvedOptions{},
			want: "ALTER INDEX [IX] ON [dbo].[T] REBUILD;",
		},
		{
			name: "rebuild_index escapes identifiers",
			op:   ddl.RebuildIndex{Schema: "dbo", Table: "T", Index: "IX]X"},
			res:  ddl.ResolvedOptions{},
			want: "ALTER INDEX [IX]]X] ON [dbo].[T] REBUILD;",
		},
		{
			name: "create_index unique with guard",
			op: ddl.CreateIndex{
				Schema: "dbo", Table: "T", Index: "IX_T",
				Columns: []string{"C1", "C2"}, Unique: true,
			},
			res: ddl.ResolvedOptions{Online: true},
			want: "IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'IX_T' AND object_id = OBJECT_ID(N'[dbo].[T]'))\n" +
				"    CREATE UNIQUE INDEX [IX_T] ON [dbo].[T] ([C1], [C2]) WITH (ONLINE = ON);",
		},
		{
			name: "alter_column online",
			op:   ddl.AlterColumn{Schema: "dbo", Table: "T", Column: "C", DataType: "NVARCHAR(200)", Nullable: false},
			res:  ddl.ResolvedOptions{Online: true},
			want: "ALTER TABLE [dbo].[T] ALTER COLUMN [C] NVARCHAR(200) NOT NULL WITH (ONLINE = ON);",
		},
		{
			name: "add_column metadata-only with constant default",
			op: ddl.AddColumn{
				Schema: "dbo", Table: "DISPATCH", Column: "PROCESSED",
				DataType: "BIT", Nullable: false, Default: &ddl.Literal{Raw: "0", String: false},
			},
			res: ddl.ResolvedOptions{},
			want: "IF COL_LENGTH(N'[dbo].[DISPATCH]', N'PROCESSED') IS NULL\n" +
				"    ALTER TABLE [dbo].[DISPATCH] ADD [PROCESSED] BIT NOT NULL DEFAULT 0;",
		},
		{
			name: "add_column string default is quoted",
			op: ddl.AddColumn{
				Schema: "dbo", Table: "T", Column: "STATUS",
				DataType: "VARCHAR(10)", Nullable: true, Default: strLiteral("active"),
			},
			res: ddl.ResolvedOptions{},
			want: "IF COL_LENGTH(N'[dbo].[T]', N'STATUS') IS NULL\n" +
				"    ALTER TABLE [dbo].[T] ADD [STATUS] VARCHAR(10) NULL DEFAULT N'active';",
		},
		{
			name: "drop_index with guard",
			op:   ddl.DropIndex{Schema: "dbo", Table: "T", Index: "IX"},
			res:  ddl.ResolvedOptions{},
			want: "IF EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'IX' AND object_id = OBJECT_ID(N'[dbo].[T]'))\n" +
				"    DROP INDEX [IX] ON [dbo].[T];",
		},
		{
			name: "add_constraint primary key with options",
			op: ddl.AddConstraint{
				Schema: "dbo", Table: "T", Constraint: "PK_T",
				Kind: "primary_key", Columns: []string{"C1", "C2"},
			},
			res: ddl.ResolvedOptions{Online: true, Resumable: true, MaxDOP: intPtr(2)},
			want: "IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE name = N'PK_T' AND parent_object_id = OBJECT_ID(N'[dbo].[T]'))\n" +
				"    ALTER TABLE [dbo].[T] ADD CONSTRAINT [PK_T] PRIMARY KEY ([C1], [C2]) WITH (ONLINE = ON, RESUMABLE = ON, MAXDOP = 2);",
		},
		{
			name: "drop_column with guard",
			op:   ddl.DropColumn{Schema: "dbo", Table: "T", Column: "C"},
			res:  ddl.ResolvedOptions{},
			want: "IF COL_LENGTH(N'[dbo].[T]', N'C') IS NOT NULL\n" +
				"    ALTER TABLE [dbo].[T] DROP COLUMN [C];",
		},
		{
			name: "drop_constraint with guard",
			op:   ddl.DropConstraint{Schema: "dbo", Table: "T", Constraint: "FK_X"},
			res:  ddl.ResolvedOptions{},
			want: "IF EXISTS (SELECT 1 FROM sys.objects WHERE name = N'FK_X' AND parent_object_id = OBJECT_ID(N'[dbo].[T]'))\n" +
				"    ALTER TABLE [dbo].[T] DROP CONSTRAINT [FK_X];",
		},
		{
			name: "rebuild_index single partition with compression",
			op:   ddl.RebuildIndex{Schema: "dbo", Table: "T", Index: "IX", Partition: intPtr(3), DataCompression: "PAGE"},
			res:  ddl.ResolvedOptions{Online: true},
			want: "ALTER INDEX [IX] ON [dbo].[T] REBUILD PARTITION = 3 WITH (ONLINE = ON, DATA_COMPRESSION = PAGE);",
		},
		{
			name: "reorganize_index plain",
			op:   ddl.ReorganizeIndex{Schema: "dbo", Table: "T", Index: "IX"},
			res:  ddl.ResolvedOptions{},
			want: "ALTER INDEX [IX] ON [dbo].[T] REORGANIZE;",
		},
		{
			name: "reorganize_index partition with lob compaction",
			op:   ddl.ReorganizeIndex{Schema: "dbo", Table: "T", Index: "IX", Partition: intPtr(2), LOBCompaction: true},
			res:  ddl.ResolvedOptions{},
			want: "ALTER INDEX [IX] ON [dbo].[T] REORGANIZE PARTITION = 2 WITH (LOB_COMPACTION = ON);",
		},
		{
			name: "rebuild_heap no options",
			op:   ddl.RebuildHeap{Schema: "dbo", Table: "T"},
			res:  ddl.ResolvedOptions{},
			want: "ALTER TABLE [dbo].[T] REBUILD;",
		},
		{
			name: "rebuild_heap online maxdop compression",
			op:   ddl.RebuildHeap{Schema: "dbo", Table: "T", DataCompression: "PAGE"},
			res:  ddl.ResolvedOptions{Online: true, MaxDOP: intPtr(4)},
			want: "ALTER TABLE [dbo].[T] REBUILD WITH (ONLINE = ON, MAXDOP = 4, DATA_COMPRESSION = PAGE);",
		},
		{
			name: "update_statistics whole table fullscan",
			op:   ddl.UpdateStatistics{Schema: "dbo", Table: "T", FullScan: true},
			res:  ddl.ResolvedOptions{},
			want: "UPDATE STATISTICS [dbo].[T] WITH FULLSCAN;",
		},
		{
			name: "update_statistics named stat with sample",
			op:   ddl.UpdateStatistics{Schema: "dbo", Table: "T", Statistic: "IX_T", SamplePercent: intPtr(30)},
			res:  ddl.ResolvedOptions{},
			want: "UPDATE STATISTICS [dbo].[T] [IX_T] WITH SAMPLE 30 PERCENT;",
		},
		{
			name: "update_statistics resample",
			op:   ddl.UpdateStatistics{Schema: "dbo", Table: "T", Resample: true},
			res:  ddl.ResolvedOptions{},
			want: "UPDATE STATISTICS [dbo].[T] WITH RESAMPLE;",
		},
		{
			name: "update_statistics default sampling",
			op:   ddl.UpdateStatistics{Schema: "dbo", Table: "T"},
			res:  ddl.ResolvedOptions{},
			want: "UPDATE STATISTICS [dbo].[T];",
		},
		{
			name: "check_db basic",
			op:   ddl.CheckDB{Database: "MYDB"},
			res:  ddl.ResolvedOptions{},
			want: "DBCC CHECKDB ([MYDB]) WITH NO_INFOMSGS, ALL_ERRORMSGS;",
		},
		{
			name: "check_db physical_only data_purity maxdop",
			op:   ddl.CheckDB{Database: "MYDB", PhysicalOnly: true, DataPurity: true},
			res:  ddl.ResolvedOptions{MaxDOP: intPtr(2)},
			want: "DBCC CHECKDB ([MYDB]) WITH NO_INFOMSGS, ALL_ERRORMSGS, PHYSICAL_ONLY, DATA_PURITY, MAXDOP = 2;",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ddl.Generate(tt.op, tt.res)
			if err != nil {
				t.Fatalf("Generate() error = %v, want nil", err)
			}
			if got != tt.want {
				t.Errorf("Generate() mismatch:\n got: %s\nwant: %s", got, tt.want)
			}
		})
	}
}

// unknownOp is an Operation the generator does not recognize.
type unknownOp struct{}

func (unknownOp) CommandType() string   { return "unknown" }
func (unknownOp) Target() ddl.ObjectRef { return ddl.ObjectRef{} }
func (unknownOp) Validate() error       { return nil }

func TestGenerateUnsupported(t *testing.T) {
	_, err := ddl.Generate(unknownOp{}, ddl.ResolvedOptions{})
	if !errors.Is(err, ddl.ErrUnsupportedOperation) {
		t.Errorf("Generate(unknownOp) error = %v, want errors.Is ErrUnsupportedOperation", err)
	}
}
