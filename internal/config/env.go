package config

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// ARICredentials は ari.env から読む ARI の認証情報。
type ARICredentials struct {
	User     string
	Password string
}

// LoadARICredentials は path の KEY=VALUE 形式のファイルから ARI_USER と ARI_PASS を読む。
func LoadARICredentials(path string) (ARICredentials, error) {
	file, err := os.Open(path)
	if err != nil {
		return ARICredentials{}, fmt.Errorf("設定ファイルがない: %s: %w", path, err)
	}
	defer file.Close()

	values := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key, value, ok := parseEnvLine(scanner.Text())
		if ok {
			values[key] = value
		}
	}
	if err := scanner.Err(); err != nil {
		return ARICredentials{}, fmt.Errorf("設定ファイルの読込失敗: %w", err)
	}

	creds := ARICredentials{User: values["ARI_USER"], Password: values["ARI_PASS"]}
	if creds.User == "" || creds.Password == "" {
		return ARICredentials{}, fmt.Errorf("ari.env に ARI_USER と ARI_PASS が必要")
	}
	return creds, nil
}

func parseEnvLine(line string) (key, value string, ok bool) {
	line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "\ufeff"))
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
	key, value, found := strings.Cut(line, "=")
	if !found {
		return "", "", false
	}
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)
	if len(value) >= 2 {
		first, last := value[0], value[len(value)-1]
		if (first == '"' || first == '\'') && first == last {
			value = value[1 : len(value)-1]
		}
	}
	return key, value, key != ""
}
