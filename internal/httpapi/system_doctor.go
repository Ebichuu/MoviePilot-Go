package httpapi

import (
	"net/http"
)

// systemDoctor 对应 V2 `app/doctor`（离线自检）的在线形态：
// 把依赖连通性、通知可投递性、对账收敛度、数据源可用性汇总成一份报告。
// 只读、只返回事实与稳定原因，不含任何凭据。
func (d Dependencies) systemDoctor(w http.ResponseWriter, r *http.Request) {
	if d.Doctor == nil {
		writeError(w, http.StatusServiceUnavailable, "自检能力未接入")
		return
	}
	writeSuccess(w, d.Doctor.Run(r.Context()))
}
