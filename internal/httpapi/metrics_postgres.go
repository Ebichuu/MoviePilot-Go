package httpapi

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
)

type postgresPoolSnapshot struct {
	acquired     int32
	idle         int32
	constructing int32
	total        int32
	max          int32
}

func registerPostgresPoolMetrics(registerer prometheus.Registerer, db *database.DB) {
	read := func() postgresPoolSnapshot {
		stats := db.Stats()
		if stats == nil {
			return postgresPoolSnapshot{}
		}
		return postgresPoolSnapshot{
			acquired:     stats.AcquiredConns(),
			idle:         stats.IdleConns(),
			constructing: stats.ConstructingConns(),
			total:        stats.TotalConns(),
			max:          stats.MaxConns(),
		}
	}

	metrics := []struct {
		name  string
		help  string
		value func(postgresPoolSnapshot) int32
	}{
		{
			name:  "moviepilot_postgres_pool_acquired_connections",
			help:  "当前已被请求占用的 PostgreSQL 连接数。",
			value: func(snapshot postgresPoolSnapshot) int32 { return snapshot.acquired },
		},
		{
			name:  "moviepilot_postgres_pool_idle_connections",
			help:  "当前空闲的 PostgreSQL 连接数。",
			value: func(snapshot postgresPoolSnapshot) int32 { return snapshot.idle },
		},
		{
			name:  "moviepilot_postgres_pool_constructing_connections",
			help:  "当前正在建立的 PostgreSQL 连接数。",
			value: func(snapshot postgresPoolSnapshot) int32 { return snapshot.constructing },
		},
		{
			name:  "moviepilot_postgres_pool_total_connections",
			help:  "当前 PostgreSQL 连接池连接总数。",
			value: func(snapshot postgresPoolSnapshot) int32 { return snapshot.total },
		},
		{
			name:  "moviepilot_postgres_pool_max_connections",
			help:  "PostgreSQL 连接池连接数硬上限。",
			value: func(snapshot postgresPoolSnapshot) int32 { return snapshot.max },
		},
	}

	for _, metric := range metrics {
		value := metric.value
		registerer.MustRegister(prometheus.NewGaugeFunc(
			prometheus.GaugeOpts{Name: metric.name, Help: metric.help},
			func() float64 { return float64(value(read())) },
		))
	}
}
