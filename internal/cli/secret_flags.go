package cli

import (
	"flag"
	"fmt"
)

func resolveSecretFlag(flags *flag.FlagSet, valueName, fileName string, value *string, filePath string) (bool, error) {
	valueSet := flagWasSet(flags, valueName)
	fileSet := flagWasSet(flags, fileName)
	if valueSet && fileSet {
		return false, fmt.Errorf("choose only one of --%s or --%s", valueName, fileName)
	}
	if !fileSet {
		return valueSet, nil
	}
	loaded, err := readSecretFile(filePath)
	if err != nil {
		return false, fmt.Errorf("read --%s: %w", fileName, err)
	}
	*value = loaded
	return true, nil
}
