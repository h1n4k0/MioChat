package main

import (
	"errors"
	"fmt"
	"strconv"
)

// Produce the same message body/kind as the TUI. The caller must still use the
// session-aware Store transaction; this helper grants no sending authority.
func guiSimpleMessage(command, arg string) (string, string, error) {
	switch command {
	case "/me":
		return arg, "action", nil
	case "/roll":
		sides := int64(6)
		if arg != "" {
			var err error
			sides, err = strconv.ParseInt(arg, 10, 64)
			if err != nil {
				return "", "", err
			}
		}
		if sides < 2 || sides > 1000000 {
			return "", "", errors.New("面数范围：2–1000000")
		}
		return fmt.Sprintf("掷出了 %d（%d 面骰）", randomNumber(sides)+1, sides), "action", nil
	case "/coin":
		result := "正面"
		if randomNumber(2) == 0 {
			result = "反面"
		}
		return "抛硬币：" + result, "action", nil
	case "/8ball":
		if arg == "" {
			return "", "", errors.New("用法：/8ball 今天能写完吗？")
		}
		answers := []string{"毫无疑问", "很有希望", "再想一想", "现在还不好说", "不如先喝杯水", "机会渺茫", "大胆去做", "问问你的直觉"}
		return "问：" + arg + " / 八号球：" + answers[randomNumber(int64(len(answers)))], "action", nil
	case "/shrug":
		return "¯\\_(ツ)_/¯", "chat", nil
	default:
		return "", "", errors.New("unknown simple message action")
	}
}
