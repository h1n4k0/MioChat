package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
)

type cachedMessage struct {
	Message                      Message
	Width                        int
	Style                        string
	Lines                        []string
	NameOffset, NameX, NameWidth int
}

func (m model) renderMessage(message Message, width int, style string) cachedMessage {
	block := cachedMessage{Message: message, Width: width, Style: style, NameWidth: ansi.StringWidth(message.Sender)}
	if message.ReplyTo > 0 {
		quote := fmt.Sprintf("  ↳ #%d %s: %s", message.ReplyTo, message.ReplyName, message.ReplyBody)
		block.Lines = append(block.Lines, m.color(ansi.Truncate(quote, width, "…"), "#70839B", false))
	}
	block.NameOffset = len(block.Lines)
	prefix := time.Unix(message.At, 0).In(time.FixedZone("CST", 8*3600)).Format("15:04") + " "
	if message.Recipient != "" {
		prefix += "[私信] "
	}
	if message.Kind == "action" {
		prefix += "* "
	}
	block.NameX = ansi.StringWidth(prefix)
	body := message.Body
	if message.Recalled > 0 {
		body = "[消息已撤回]"
	}
	if message.Edited > 0 && message.Recalled == 0 {
		body += " [已编辑]"
	}
	if message.ReactionSummary != "" {
		body += " [" + message.ReactionSummary + "]"
	}
	body += m.color(fmt.Sprintf(" ·%d", message.ID), "#526078", false)
	text := m.color(prefix, "#70839B", false) + m.color(message.Sender, m.accent(), true) + " " + body
	block.Lines = append(block.Lines, strings.Split(ansi.Hardwrap(text, width, true), "\n")...)
	return block
}
