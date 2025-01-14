/*
Copyright 2019 The Kubernetes Authors.

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

package pjutil

import (
	"fmt"
	"regexp"

	"github.com/sirupsen/logrus"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/test-infra/prow/config"
)

var TestAllRe = regexp.MustCompile(`(?m)^/test all,?($|\s.*)`)

// RetestRe provides the regex for `/retest`
var RetestRe = regexp.MustCompile(`(?m)^/retest\s*$`)

// RetestRe provides the regex for `/retest-required`
var RetestRequiredRe = regexp.MustCompile(`(?m)^/retest-required\s*$`)

var OkToTestRe = regexp.MustCompile(`(?m)^/ok-to-test\s*$`)

// AvailablePresubmits returns 3 sets of presubmits:
// 1. presubmits that can be run with '/test all' command.
// 2. optional presubmits commands that can be run with their trigger, e.g. '/test job'
// 3. required presubmits commands that can be run with their trigger, e.g. '/test job'
func AvailablePresubmits(changes config.ChangedFilesProvider, org, repo, branch string,
	presubmits []config.Presubmit, logger *logrus.Entry) (sets.String, sets.String, sets.String, error) {
	runWithTestAllNames := sets.NewString()
	optionalJobTriggerCommands := sets.NewString()
	requiredJobsTriggerCommands := sets.NewString()

	runWithTestAll, err := FilterPresubmits(TestAllFilter(), changes, branch, presubmits, logger)
	if err != nil {
		return runWithTestAllNames, optionalJobTriggerCommands, requiredJobsTriggerCommands, err
	}

	var triggerFilters []Filter
	for _, ps := range presubmits {
		triggerFilters = append(triggerFilters, CommandFilter(ps.RerunCommand))
	}
	runWithTrigger, err := FilterPresubmits(AggregateFilter(triggerFilters), changes, branch, presubmits, logger)
	if err != nil {
		return runWithTestAllNames, optionalJobTriggerCommands, requiredJobsTriggerCommands, err
	}

	for _, ps := range runWithTestAll {
		runWithTestAllNames.Insert(ps.Name)
	}

	for _, ps := range runWithTrigger {
		if ps.Optional {
			optionalJobTriggerCommands.Insert(ps.RerunCommand)
		} else {
			requiredJobsTriggerCommands.Insert(ps.RerunCommand)
		}
	}

	return runWithTestAllNames, optionalJobTriggerCommands, requiredJobsTriggerCommands, nil
}

// Filter digests a presubmit config to determine if:
//  - the presubmit matched the filter
//  - we know that the presubmit is forced to run
//  - what the default behavior should be if the presubmit
//    runs conditionally and does not match trigger conditions
type Filter func(p config.Presubmit) (shouldRun bool, forcedToRun bool, defaultBehavior bool)

// CommandFilter builds a filter for `/test foo`
func CommandFilter(body string) Filter {
	return func(p config.Presubmit) (bool, bool, bool) {
		return p.TriggerMatches(body), p.TriggerMatches(body), true
	}
}

// TestAllFilter builds a filter for the automatic behavior of `/test all`.
// Jobs that explicitly match `/test all` in their trigger regex will be
// handled by a commandFilter for the comment in question.
func TestAllFilter() Filter {
	return func(p config.Presubmit) (bool, bool, bool) {
		return !p.NeedsExplicitTrigger(), false, false
	}
}

// AggregateFilter builds a filter that evaluates the child filters in order
// and returns the first match
func AggregateFilter(filters []Filter) Filter {
	return func(presubmit config.Presubmit) (bool, bool, bool) {
		for _, filter := range filters {
			if shouldRun, forced, defaults := filter(presubmit); shouldRun {
				return shouldRun, forced, defaults
			}
		}
		return false, false, false
	}
}

// FilterPresubmits determines which presubmits should run by evaluating the user-provided filter.
func FilterPresubmits(filter Filter, changes config.ChangedFilesProvider, branch string, presubmits []config.Presubmit, logger logrus.FieldLogger) ([]config.Presubmit, error) {

	var toTrigger []config.Presubmit
	var namesToTrigger []string
	var noMatch, shouldnotRun int
	for _, presubmit := range presubmits {
		matches, forced, defaults := filter(presubmit)
		if !matches {
			noMatch++
			continue
		}
		shouldRun, err := presubmit.ShouldRun(branch, changes, forced, defaults)
		if err != nil {
			return nil, fmt.Errorf("%s: should run: %w", presubmit.Name, err)
		}
		if !shouldRun {
			shouldnotRun++
			continue
		}
		toTrigger = append(toTrigger, presubmit)
		namesToTrigger = append(namesToTrigger, presubmit.Name)
	}

	logger.WithFields(logrus.Fields{
		"to-trigger":           namesToTrigger,
		"total-count":          len(presubmits),
		"to-trigger-count":     len(toTrigger),
		"no-match-count":       noMatch,
		"should-not-run-count": shouldnotRun}).Debug("Filtered complete.")
	return toTrigger, nil
}

// RetestFilter builds a filter for `/retest`
func RetestFilter(failedContexts, allContexts sets.String) Filter {
	return func(p config.Presubmit) (bool, bool, bool) {
		failed := failedContexts.Has(p.Context)
		return failed || (!p.NeedsExplicitTrigger() && !allContexts.Has(p.Context)), false, failed
	}
}

func RetestRequiredFilter(failedContext, allContexts sets.String) Filter {
	return func(ps config.Presubmit) (bool, bool, bool) {
		if ps.Optional {
			return false, false, false
		}
		return RetestFilter(failedContext, allContexts)(ps)
	}
}

type contextGetter func() (sets.String, sets.String, error)

// PresubmitFilter creates a filter for presubmits
func PresubmitFilter(honorOkToTest bool, contextGetter contextGetter, body string, logger logrus.FieldLogger) (Filter, error) {
	// the filters determine if we should check whether a job should run, whether
	// it should run regardless of whether its triggering conditions match, and
	// what the default behavior should be for that check. Multiple filters
	// can match a single presubmit, so it is important to order them correctly
	// as they have precedence -- filters that override the false default should
	// match before others. We order filters by amount of specificity.
	var filters []Filter
	filters = append(filters, CommandFilter(body))
	if RetestRe.MatchString(body) {
		logger.Info("Using retest filter.")
		failedContexts, allContexts, err := contextGetter()
		if err != nil {
			return nil, err
		}
		filters = append(filters, RetestFilter(failedContexts, allContexts))
	}
	if RetestRequiredRe.MatchString(body) {
		logger.Info("Using retest-required filter")
		failedContexts, allContexts, err := contextGetter()
		if err != nil {
			return nil, err
		}
		filters = append(filters, RetestRequiredFilter(failedContexts, allContexts))
	}
	if (honorOkToTest && OkToTestRe.MatchString(body)) || TestAllRe.MatchString(body) {
		logger.Debug("Using test-all filter.")
		filters = append(filters, TestAllFilter())
	}
	return AggregateFilter(filters), nil
}
