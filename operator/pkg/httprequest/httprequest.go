// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package httprequest

import (
	"fmt"
	"io"
	"net/http"
)

// Get sends an HTTP GET request and returns the result.
func Get(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch URL %s : %s", url, resp.Status)
	}
	// Limit requests to 10mb; we expect response to be much smaller
	ret, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024*10))
	if err != nil {
		return nil, err
	}
	return ret, nil
}
