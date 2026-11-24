package app

import (
	"os"
	"strings"
	"time"
)

type moduleCard struct {
	path, description string
	timer             *time.Timer
}

func (e *Engine) SetModuleCard(path string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.card.path = path
	e.updateModuleCardLocked()
}

func (e *Engine) updateModuleCardLocked() {
	card := &e.card
	if card.path == "" {
		return
	}
	phase, label, _ := e.displayStateLocked()
	description := cardDescription(phase, label)
	if phase != "working" || card.description == "" || description == card.description {
		card.stopTimer()
		card.publish(description)
		return
	}
	if card.timer != nil {
		return
	}
	// Short polling cycles should leave the stable card status visible.
	var timer *time.Timer
	timer = time.AfterFunc(750*time.Millisecond, func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		if card.timer == timer {
			card.timer = nil
			phase, label, _ := e.displayStateLocked()
			card.publish(cardDescription(phase, label))
		}
	})
	card.timer = timer
}

func (e *Engine) stopModuleCard() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.card.stopTimer()
	e.card.publish(cardDescription("stopped", "已停止"))
	e.card.path = ""
}

func (c *moduleCard) stopTimer() {
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
}

func cardDescription(phase, label string) string {
	icon := "🟡"
	switch phase {
	case "stopped":
		icon = "⚪"
	case "running":
		icon = "🟢"
	case "error":
		icon = "🔴"
	}
	return icon + " " + label + " · 华为云 AAAA"
}

func (c *moduleCard) publish(description string) {
	if c.path == "" || c.description == description {
		return
	}
	if err := writeModuleDescription(c.path, description); err == nil {
		c.description = description
	}
}

func writeModuleDescription(path, description string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	key := "description="
	entry := key + description
	lines := strings.SplitAfter(string(data), "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, key) {
			if line == entry+"\n" {
				return nil
			}
			lines[i] = entry + "\n"
			return writeAtomic(path, []byte(strings.Join(lines, "")), info.Mode().Perm())
		}
	}
	return writeAtomic(path, []byte(strings.TrimSuffix(string(data), "\n")+"\n"+entry+"\n"), info.Mode().Perm())
}
