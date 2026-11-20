package notificationconfig

// Select returns detached Telegram targets in configuration order. A directed
// user notification bypasses switch filtering but still obeys channel and
// source selection.
func (s Snapshot) Select(channel, source, mtype, userid string) []Telegram {
	if channel != "" && channel != TelegramType {
		return nil
	}
	selected := make([]Telegram, 0, len(s.Telegrams))
	for _, configured := range s.Telegrams {
		if source != "" && source != configured.Name {
			continue
		}
		if userid == "" && mtype != "" && !SwitchMatches(configured.Switchs, mtype) {
			continue
		}
		selected = append(selected, configured.Clone())
	}
	return selected
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
