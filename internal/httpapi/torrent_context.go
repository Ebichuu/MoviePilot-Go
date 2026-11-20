package httpapi

import (
	"fmt"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
)

func optionalText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func normalizedLabels(labels []string) []string {
	if len(labels) == 0 {
		return []string{}
	}
	result := make([]string, 0, len(labels))
	for _, label := range labels {
		if label != "" {
			result = append(result, label)
		}
	}
	return result
}

func volumeFactor(resource indexer.Resource) string {
	if resource.DownloadVolumeFactor == nil || resource.UploadVolumeFactor == nil {
		return "未知"
	}
	known := map[string]string{
		"1.00 1.00": "普通",
		"1.00 0.00": "免费",
		"2.00 1.00": "2X",
		"4.00 1.00": "4X",
		"2.00 0.00": "2X免费",
		"4.00 0.00": "4X免费",
		"1.00 0.50": "50%",
		"2.00 0.50": "2X 50%",
		"1.00 0.70": "70%",
		"1.00 0.30": "30%",
		"1.00 0.75": "75%",
		"1.00 0.25": "25%",
	}
	if value, ok := known[fmt.Sprintf("%.2f %.2f", *resource.UploadVolumeFactor, *resource.DownloadVolumeFactor)]; ok {
		return value
	}
	return "未知"
}

func freedateDiff(value time.Time, location *time.Location) string {
	if value.IsZero() {
		return ""
	}
	if location == nil {
		location = time.UTC
	}
	diff := value.In(location).Sub(time.Now().In(location))
	if diff <= 0 {
		return ""
	}
	minutes := int(diff / time.Minute)
	if minutes < 1 {
		return ""
	}
	days, hours := minutes/(24*60), (minutes/60)%24
	minutes %= 60
	if days > 0 {
		return fmt.Sprintf("%d天%d小时%d分钟", days, hours, minutes)
	}
	if hours > 0 {
		return fmt.Sprintf("%d小时%d分钟", hours, minutes)
	}
	return fmt.Sprintf("%d分钟", minutes)
}
