package main

import (
	"fmt"
	"strings"

	"pr-agent-go/internal/config"
	"pr-agent-go/internal/processor"
	"pr-agent-go/internal/status"
	"pr-agent-go/internal/storage"
)

func runStatus(cfg config.Config, store *storage.FileStorage, jsonOutput bool) error {
	overview, err := status.FetchRemoteOverview(cfg.Port)
	if err != nil {
		overview, err = status.BuildOverview(cfg, store, processor.Snapshot{}, status.RunChecks(cfg))
		if err != nil {
			return err
		}
	}

	if jsonOutput {
		return printJSON(overview)
	}

	fmt.Printf("Service: %s\n", overview.Service)
	fmt.Printf("Time: %s\n", overview.Now)
	fmt.Printf("Port: %d\n", overview.Config.Port)
	fmt.Printf("Workers: %d\n", overview.Config.WorkerCount)
	fmt.Printf("Queue: queued=%d active=%d completed=%d failed=%d\n", overview.Queue.Queued, overview.Queue.Active, overview.Queue.Completed, overview.Queue.Failed)
	fmt.Printf("GitHub: %s\n", summarizeCheck(overview.Checks, "github"))
	fmt.Printf("Model: %s\n", summarizeCheck(overview.Checks, "model"))
	fmt.Printf("Webhook Secret: %t\n", overview.Config.WebhookSecured)
	fmt.Printf("Today: reviews=%d failed_events=%d repos=%s\n", overview.Daily.ReviewCount, overview.Daily.FailedEvents, strings.Join(overview.Daily.Repos, ", "))
	if overview.ReviewMetrics.LatestRunAt != "" {
		fmt.Printf("Latest PR: %s #%d at %s\n", overview.ReviewMetrics.LatestRepo, overview.ReviewMetrics.LatestPR, overview.ReviewMetrics.LatestRunAt)
	}
	if len(overview.Queue.RecentFailures) > 0 {
		fmt.Printf("Recent Failures:\n")
		for _, failure := range overview.Queue.RecentFailures {
			fmt.Printf("- %s\n", failure)
		}
	}
	return nil
}

func runStats(cfg config.Config, store *storage.FileStorage, jsonOutput bool) error {
	overview, err := status.FetchRemoteOverview(cfg.Port)
	if err != nil {
		overview, err = status.BuildOverview(cfg, store, processor.Snapshot{}, status.RunChecks(cfg))
		if err != nil {
			return err
		}
	}

	if jsonOutput {
		return printJSON(map[string]any{
			"daily":         overview.Daily,
			"reviewMetrics": overview.ReviewMetrics,
			"eventMetrics":  overview.EventMetrics,
			"queue":         overview.Queue,
			"recentRuns":    overview.RecentRuns,
		})
	}

	fmt.Printf("Daily Reviews: %d\n", overview.Daily.ReviewCount)
	fmt.Printf("Risk Counts: low=%d medium=%d high=%d unknown=%d\n",
		overview.Daily.RiskCounts["low"],
		overview.Daily.RiskCounts["medium"],
		overview.Daily.RiskCounts["high"],
		overview.Daily.RiskCounts["unknown"],
	)
	fmt.Printf("All-time Reviews: %d\n", overview.ReviewMetrics.Total)
	fmt.Printf("Event Statuses: %s\n", formatCountMap(overview.EventMetrics.ByStatus))
	fmt.Printf("Review Statuses: %s\n", formatCountMap(overview.ReviewMetrics.ByStatus))
	fmt.Printf("Queue: queued=%d active=%d completed=%d failed=%d\n", overview.Queue.Queued, overview.Queue.Active, overview.Queue.Completed, overview.Queue.Failed)
	if len(overview.RecentRuns) > 0 {
		fmt.Printf("Recent Runs:\n")
		for _, run := range overview.RecentRuns {
			fmt.Printf("- %s %s #%d risk=%s trust=%s action=%s/%s provider=%s at %s\n",
				run.CreatedAt, run.RepoFullName, run.PRNumber, run.OverallRisk, emptyDash(run.TrustLevel), emptyDash(run.ActionTaken), emptyDash(run.ActionStatus), run.Provider, run.CreatedAt)
		}
	}
	return nil
}

func runDoctor(cfg config.Config, jsonOutput bool) error {
	checks := status.RunChecks(cfg)
	if jsonOutput {
		return printJSON(checks)
	}

	for _, check := range checks {
		state := "FAIL"
		if check.OK {
			state = "OK"
		}
		fmt.Printf("%s %s: %s\n", state, check.Name, check.Message)
	}
	return nil
}

func runLogs(_ config.Config, store *storage.FileStorage, jsonOutput bool) error {
	eventLogs, err := store.ListEventLogs()
	if err != nil {
		return err
	}
	reviewRuns, err := store.ListReviewRuns()
	if err != nil {
		return err
	}

	recentEvents := tailEvents(eventLogs, 10)
	recentRuns := tailRuns(reviewRuns, 10)

	if jsonOutput {
		return printJSON(map[string]any{
			"recentEvents": recentEvents,
			"recentRuns":   recentRuns,
		})
	}

	fmt.Println("Recent Event Logs:")
	if len(recentEvents) == 0 {
		fmt.Println("- none")
	} else {
		for _, logEntry := range recentEvents {
			fmt.Printf("- %s %s repo=%s status=%s reason=%s error=%s\n",
				logEntry.ReceivedAt, logEntry.EventName, logEntry.RepoFullName, logEntry.ProcessStatus, emptyDash(logEntry.Reason), emptyDash(logEntry.ErrorMessage))
		}
	}

	fmt.Println("Recent Review Runs:")
	if len(recentRuns) == 0 {
		fmt.Println("- none")
	} else {
		for _, run := range recentRuns {
			fmt.Printf("- %s %s #%d risk=%s trust=%s action=%s/%s trigger=%s timings=%s\n",
				run.CreatedAt,
				run.RepoFullName,
				run.PRNumber,
				run.OverallRisk,
				emptyDash(run.TrustLevel),
				emptyDash(run.ActionTaken),
				emptyDash(run.ActionStatus),
				run.TriggerEvent,
				formatStageDurations(run.StageDurationsMS),
			)
		}
	}
	return nil
}
