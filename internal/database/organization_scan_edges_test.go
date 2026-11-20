package database

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
)

// orgBatchScanRow 是按列复刻 scanOrganizationBatch 目标类型的行替身。
type orgBatchScanRow struct {
	values []any
}

func (r orgBatchScanRow) Scan(dest ...any) error {
	if len(dest) != len(r.values) {
		return errors.New("列数与目标不匹配")
	}
	for index, value := range r.values {
		switch target := dest[index].(type) {
		case *uuid.UUID:
			*target = value.(uuid.UUID)
		case *OrganizationBatchState:
			*target = value.(OrganizationBatchState)
		case *string:
			*target = value.(string)
		case *[]byte:
			*target = value.([]byte)
		case *int:
			*target = value.(int)
		case *time.Time:
			*target = value.(time.Time)
		case **uuid.UUID:
			*target = value.(*uuid.UUID)
		case **time.Time:
			*target = value.(*time.Time)
		default:
			return errors.New("不支持的目标类型")
		}
	}
	return nil
}

func organizationBatchScanValues(state OrganizationBatchState, sourceRoot, targetRoot string, encoded []byte) []any {
	now := time.Now().UTC()
	return []any{
		uuid.New(),
		uuid.New(),
		state,
		sourceRoot,
		targetRoot,
		encoded,
		"",
		0,
		now,
		(*uuid.UUID)(nil),
		(*time.Time)(nil),
		(*time.Time)(nil),
		now,
		now,
	}
}

func TestScanOrganizationBatchRejectsMalformedPayload(t *testing.T) {
	row := orgBatchScanRow{values: organizationBatchScanValues(
		OrganizationBatchPlanned, `C:\downloads`, `C:\media\lib`, []byte("{"),
	)}
	if _, err := scanOrganizationBatch(row); !errors.Is(err, ErrOrganizationPersistence) {
		t.Fatalf("非法计划载荷 error = %v", err)
	}
}

func TestScanOrganizationBatchRejectsSourceOutsideRoot(t *testing.T) {
	operations := []organizer.Operation{{
		Source:         `C:\elsewhere\a.mkv`,
		Target:         `C:\media\lib\a.mkv`,
		Mode:           organizer.Copy,
		ExpectedSize:   1,
		ExpectedSHA256: strings.Repeat("a", 64),
	}}
	encoded, err := json.Marshal(operations)
	if err != nil {
		t.Fatalf("编码计划: %v", err)
	}
	row := orgBatchScanRow{values: organizationBatchScanValues(
		OrganizationBatchPlanned, `C:\downloads`, `C:\media\lib`, encoded,
	)}
	if _, err := scanOrganizationBatch(row); !errors.Is(err, ErrOrganizationPersistence) {
		t.Fatalf("源文件越界 error = %v", err)
	}
}

func TestScanOrganizationBatchAcceptsValidBatch(t *testing.T) {
	operations := []organizer.Operation{{
		Source:         `C:\downloads\a.mkv`,
		Target:         `C:\media\lib\a.mkv`,
		Mode:           organizer.Copy,
		ExpectedSize:   1,
		ExpectedSHA256: strings.Repeat("b", 64),
	}}
	encoded, err := json.Marshal(operations)
	if err != nil {
		t.Fatalf("编码计划: %v", err)
	}
	row := orgBatchScanRow{values: organizationBatchScanValues(
		OrganizationBatchPlanned, `C:\downloads`, `C:\media\lib`, encoded,
	)}
	batch, err := scanOrganizationBatch(row)
	if err != nil {
		t.Fatalf("合法批次 error = %v", err)
	}
	if len(batch.Plan.Operations) != 1 {
		t.Fatalf("计划操作数 = %d", len(batch.Plan.Operations))
	}
}
