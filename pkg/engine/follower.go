// Copyright: This file is part of korrel8r, released under https://github.com/korrel8r/korrel8r/blob/main/LICENSE

package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"

	"github.com/korrel8r/korrel8r/pkg/graph"
	"github.com/korrel8r/korrel8r/pkg/korrel8r"
)

type appliedRule struct {
	Rule  korrel8r.Rule
	Start korrel8r.Class
}

// Follower provides Vist() and Traverse() methods to follow rules and collect results in a graph.
type Follower struct {
	Engine     *Engine
	Context    context.Context
	Constraint *korrel8r.Constraint
	Err        error // Collect errors using errors.Join

	// temporary store for results of rules that need to be saved for a later line.
	rules map[appliedRule]graph.Queries
}

// Traverse a line gets all queries provided by Visit() on the From node,
// and stores results on the To node.
func (f *Follower) Traverse(l *graph.Line) bool {
	rule := graph.RuleFor(l)
	start, goal := l.From().(*graph.Node), l.To().(*graph.Node)
	log := log.WithValues("rule", rule.Name(), "start", start.Class.String(), "goal", goal.Class.String())

	// Check if rule was already applied with this start class.
	key := appliedRule{Start: start.Class, Rule: rule}
	if _, applied := f.rules[key]; !applied { // Not yet applied.
		f.rules[key] = graph.Queries{}
		for _, s := range start.Result.List() { // Apply to each start object
			q, err := rule.Apply(s)
			if err != nil {
				f.Err = errors.Join(f.Err, fmt.Errorf("Error applying rule %v(%v): %w", rule.Name(), start.Class, err))
				log.V(2).Info("Error applying rule", "error", err)
				continue
			}
			f.rules[key].Set(q, -1)
		}
	}
	// Take queries that match this line's goal, leave the rest.
	maps.DeleteFunc(f.rules[key], func(s string, qc graph.QueryCount) bool {
		q := qc.Query
		log := log.WithValues("query", q.String())
		switch {
		case q.Class() != goal.Class: // Wrong goal, leave it for another line.
			return false
		case goal.Queries.Has(q): // Already evaluated on goal node.
			l.Queries.Set(q, qc.Count) // Record on the link
			return true
		default: // Evaluate the query and store the results
			result := korrel8r.NewCountResult(goal.Result) // Store in goal, but count the contribution.
			if err := f.Engine.Get(f.Context, q, f.Constraint, result); err != nil {
				// TODO distinguish between expected "not found" errors and unexpected "can't talk to store" errors.
				log.V(2).Info("Get error", "error", err)
			}
			l.Queries.Set(q, result.Count)
			goal.Queries.Set(q, result.Count)
			if result.Count > 0 {
				log.V(3).Info("Query result", "count", result.Count)
			}
			return true
		}
	})
	return l.Queries.Total() > 0
}
