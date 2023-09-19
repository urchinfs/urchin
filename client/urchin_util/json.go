package urchin_util

import (
	"bytes"
	"encoding/json"
)

func Marshal(data interface{}) ([]byte, error) {
	bf := bytes.NewBuffer([]byte{})
	jsonEncoder := json.NewEncoder(bf)
	jsonEncoder.SetEscapeHTML(false)
	if err := jsonEncoder.Encode(data); err != nil {
		return nil, err
	}

	return bf.Bytes(), nil
}
