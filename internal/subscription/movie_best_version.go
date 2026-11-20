package subscription

// MovieUpgradeAllowed mirrors V2's movie gate after filter groups assign priority.
// A missing or zero baseline imposes no threshold, including for negative candidates.
func MovieUpgradeAllowed(bestVersion bool, currentPriority *int, candidatePriority int) bool {
	return !bestVersion || currentPriority == nil || *currentPriority == 0 ||
		candidatePriority > *currentPriority
}

// MovieBestVersionComplete requires V2's exact terminal priority, not a minimum.
func MovieBestVersionComplete(bestVersion bool, currentPriority *int) bool {
	return bestVersion && currentPriority != nil && *currentPriority == 100
}
