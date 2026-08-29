package pipeline

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// ParseSpec decodes a pipeline YAML spec, applies defaults (worktree=none,
// type=development, all statuses pending), sets ID=Name, and validates the DAG.
func ParseSpec(data []byte) (*Pipeline, error) {
	var p Pipeline
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse pipeline yaml: %w", err)
	}
	p.ID = p.Name
	p.Status = StatusPending
	for i := range p.Jobs {
		j := &p.Jobs[i]
		if j.Worktree == "" {
			j.Worktree = "pipeline"
		}
		if j.Type == "" {
			j.Type = "development"
		}
		if j.RunIf == "" {
			j.RunIf = "success"
		}
		j.Status = JobPending
	}
	injectSpanNodes(&p)
	if err := Validate(&p); err != nil {
		return nil, err
	}
	return &p, nil
}

func injectSpanNodes(p *Pipeline) {
	// 1. Identify roots and inject root-span-out
	inDegree := make(map[string]int)
	for i := range p.Jobs {
		job := &p.Jobs[i]
		for range job.DependsOn {
			inDegree[job.ID]++
		}
	}

	rootSpanOutID := "root-span-out"
	rootSpanOutJob := Job{
		ID:       rootSpanOutID,
		Type:     "span-out",
		Status:   JobPending,
		Worktree: "pipeline",
		System:   true,
	}
	var newJobs []Job
	newJobs = append(newJobs, rootSpanOutJob)

	for i := range p.Jobs {
		job := &p.Jobs[i]
		if inDegree[job.ID] == 0 {
			job.DependsOn = append(job.DependsOn, rootSpanOutID)
		}
		newJobs = append(newJobs, *job)
	}
	p.Jobs = newJobs

	// 2. Identify mid-graph span-outs
	children := make(map[string][]string)
	for i := range p.Jobs {
		job := &p.Jobs[i]
		for _, dep := range job.DependsOn {
			children[dep] = append(children[dep], job.ID)
		}
	}

	var jobsWithSpanOuts []Job
	for i := range p.Jobs {
		job := &p.Jobs[i]
		jobsWithSpanOuts = append(jobsWithSpanOuts, *job)

		if len(children[job.ID]) > 1 && job.ID != rootSpanOutID {
			spanOutID := job.ID + "-span-out"
			spanOutJob := Job{
				ID:        spanOutID,
				Type:      "span-out",
				DependsOn: []string{job.ID},
				Status:    JobPending,
				Worktree:  "pipeline",
				System:    true,
			}
			jobsWithSpanOuts = append(jobsWithSpanOuts, spanOutJob)
		}
	}
	p.Jobs = jobsWithSpanOuts

	// Wire span-outs
	for i := range p.Jobs {
		job := &p.Jobs[i]
		if job.Type == "span-out" {
			continue
		}
		var newDeps []string
		for _, dep := range job.DependsOn {
			if len(children[dep]) > 1 && dep != rootSpanOutID {
				newDeps = append(newDeps, dep+"-span-out")
			} else {
				newDeps = append(newDeps, dep)
			}
		}
		job.DependsOn = newDeps
	}

	// 3. Identify span-ins
	inDegree2 := make(map[string]int)
	for i := range p.Jobs {
		job := &p.Jobs[i]
		inDegree2[job.ID] = len(job.DependsOn)
	}

	var finalJobs []Job
	for i := range p.Jobs {
		job := &p.Jobs[i]
		if job.Type == "span-out" {
			finalJobs = append(finalJobs, *job)
			continue
		}
		if inDegree2[job.ID] > 1 {
			spanInID := job.ID + "-span-in"
			spanInJob := Job{
				ID:        spanInID,
				Type:      "span-in",
				DependsOn: job.DependsOn,
				Status:    JobPending,
				Worktree:  "pipeline",
				System:    true,
			}
			finalJobs = append(finalJobs, spanInJob)
			job.DependsOn = []string{spanInID}
		}
		finalJobs = append(finalJobs, *job)
	}
	p.Jobs = finalJobs
}
