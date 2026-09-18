package probe

import "encoding/json"

func marshalJSON(v interface{}) ([]byte, error) { return json.Marshal(v) }

func unmarshalJSON(data []byte, v interface{}) error { return json.Unmarshal(data, v) }
