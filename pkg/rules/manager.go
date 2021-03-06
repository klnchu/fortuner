package rules

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/sak0/fortuner/pkg/rulefmt"
)

var defaultInterval = 30 * time.Second

type NotifyFunc func(ctx context.Context, alerts ...*Alert)

type Group struct {
	name     string
	file     string
	interval time.Duration
	rules    []Rule
	opts     ManagerOpts

	done chan interface{}
}

func (g *Group) Stop() {
	g.done <- struct{}{}
}

func (g *Group) Run() {
	tick := time.NewTicker(g.interval)
	defer tick.Stop()
	defer glog.V(2).Infof("Group %s goroutine exit.\n", g.name)

	g.Eval(time.Now())

	for {
		select {
		case <-g.done:
			return
		case <-tick.C:
			glog.V(2).Infof("group %s with file %s Eval", g.name, g.file)
			g.Eval(time.Now().UTC())
		}
	}
}

func (g *Group) Eval(ts time.Time) {
	genAlerts := func(ctx context.Context, alerts []*Alert) chan interface{} {
		outStream := make(chan interface{})

		go func() {
			defer close(outStream)
			select {
			case <-ctx.Done():
				return
			default:
				for _, alert := range alerts {
					outStream <- alert
				}
			}
		}()
		return outStream
	}

	needSending := func(ctx context.Context, input <-chan interface{}, resendDelay time.Duration, ts time.Time) chan interface{} {
		outStream := make(chan interface{})
		go func() {
			defer close(outStream)
			for {
				select {
				case <-ctx.Done():
					return
				case in, ok := <-input:
					if !ok {
						return
					}
					ar := in.(*Alert)
					if ar.State == StatePending {
						glog.V(2).Infof("Do not send pending alert.")
					}
					if ar.ResolvedAt.After(ar.LastSentAt) {
						outStream <- ar
						return
					}
					if ar.LastSentAt.Add(resendDelay).Before(ts) {
						outStream <- ar
						return
					}
					glog.V(2).Infof("Alert can not send: %#v", ar)
				}
			}
		}()
		return outStream
	}

	select {
	case <-g.done:
		return
	default:
		var alerts []*Alert
		for _, rule := range g.rules {
			if err := rule.DetermineIndex(g.opts.EnableFuzzyIndex); err != nil {
				glog.V(2).Infof("rule(%s) determine index failed: %v", rule.Name(), err)
				continue
			}
			if err := rule.Eval(g.opts.Ctx, time.Now()); err != nil {
				glog.V(2).Infof("rule %s eval failed: %v", rule.Name(), err)
				continue
			}

			ctx, cancel := context.WithCancel(g.opts.Ctx)
			for obj := range needSending(ctx, genAlerts(ctx, rule.ActiveAlerts()), g.opts.ResendDelay, ts) {
				alert := obj.(*Alert)
				alerts = append(alerts, alert)
			}
			cancel()
		}

		g.opts.NotifyFunc(g.opts.Ctx, alerts...)
	}
}

func NewGroup(opts ManagerOpts, groupName string, fileName string, rules []Rule) *Group {
	return &Group{
		interval: opts.Interval,
		name:     groupName,
		file:     fileName,
		rules:    rules,
		done:     make(chan interface{}),
		opts:     opts,
	}
}

type ManagerOpts struct {
	RulesFilePath    string
	Ctx              context.Context
	ExternalURL      *url.URL
	NotifyFunc       NotifyFunc
	Interval         time.Duration
	ResendDelay      time.Duration
	TailTime         time.Duration
	EnableFuzzyIndex bool
}

type RuleManager struct {
	mtx        sync.RWMutex
	opts       ManagerOpts
	Groups     map[string]*Group
	needUpdate int
}

func groupKey(name, file string) string {
	return name + ";" + file
}

func (m *RuleManager) Lock() {
	m.mtx.Lock()
}

func (m *RuleManager) UnLock() {
	m.mtx.Unlock()
}

func (m *RuleManager) SetNeedUpdate() {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	m.needUpdate = 1
}

func (m *RuleManager) NeedUpdate() bool {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	if m.needUpdate == 1 {
		return true
	}
	return false
}

func (m *RuleManager) CleanNeedUpdate() {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	m.needUpdate = 0
}

func (m *RuleManager) LoadGroups(fileNames []string) (map[string]*Group, error) {
	allGroups := make(map[string]*Group)

	for _, file := range fileNames {
		groups, err := rulefmt.ParseFile(file)
		if err != nil {
			glog.Fatalf("parse rule file %s failed: %v\n", file, err)
		}
		for _, grp := range groups.Groups {
			var rules []Rule
			if grp.Interval == 0 {
				grp.Interval = m.opts.Interval
			}

			for _, rule := range grp.Rules {
				if err := rule.Validate(); err != nil {
					glog.Fatalf("validate rule %s failed: %v\n", rule.Alert, err)
				}
				switch rule.Type {
				case rulefmt.RuleTypes[rulefmt.TypeFrequency]:
					newRule := NewFrequencyRule(rule, grp.Interval)
					rules = append(rules, newRule)
				case rulefmt.RuleTypes[rulefmt.TypeAny]:
					newRule := NewAnyRule(rule, grp.Interval)
					rules = append(rules, newRule)
				case rulefmt.RuleTypes[rulefmt.TypeBlackList]:
					newRule := NewWhiteListRule(rule, grp.Interval, m.opts.TailTime)
					rules = append(rules, newRule)
				case rulefmt.RuleTypes[rulefmt.TypeWhiteList]:
					newRule := NewWhiteListRule(rule, grp.Interval, m.opts.TailTime)
					rules = append(rules, newRule)
				default:
					glog.V(2).Infof("Unsupport rule type: %s\n", rule.Type)
					continue
				}
			}
			newGroup := NewGroup(m.opts, grp.Name, file, rules)
			allGroups[groupKey(grp.Name, file)] = newGroup
		}
	}

	return allGroups, nil
}

func (m *RuleManager) Update() {
	m.needUpdate = 0

	var files []string
	err := filepath.Walk(m.opts.RulesFilePath, func(path string, info os.FileInfo, err error) error {
		isYml, err := filepath.Match("*.yml", info.Name())
		if err != nil {
			return err
		}
		if isYml {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		glog.Fatalf("%v", err)
	}

	//for _, file := range m.opts.RulesFiles {
	//	matches, err := filepath.Glob(file)
	//	if err != nil {
	//		continue
	//	}
	//	for _, f := range matches {
	//		files = append(files, f)
	//	}
	//}
	glog.V(2).Infof("Update by rule files: %v\n", files)

	newGroups, err := m.LoadGroups(files)
	if err != nil {
		msg := "unexpected error, please report bug."
		if ruleErr, ok := err.(rulefmt.LowRuleError); ok {
			msg = ruleErr.Msg
		}
		rulefmt.HandleError(err, msg)
	}

	glog.V(3).Infof("Old groups: %#v\n", m.Groups)
	glog.V(3).Infof("New groups: %#v\n", newGroups)

	var wg sync.WaitGroup

	for key, newGroup := range newGroups {
		wg.Add(1)
		oldGroup, ok := m.Groups[key]
		if ok {
			glog.V(3).Infof("Group[%s] with file %s need stop.\n", key, oldGroup.file)
			delete(m.Groups, key)
			oldGroup.Stop()
		}
		go newGroup.Run()
		wg.Done()
	}

	wg.Wait()

	for _, oldGroup := range m.Groups {
		oldGroup.Stop()
	}

	m.Groups = newGroups
}

func NewRuleManager(opts ManagerOpts) *RuleManager {
	return &RuleManager{
		mtx:        sync.RWMutex{},
		opts:       opts,
		needUpdate: 0,
	}
}
