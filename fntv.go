package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// 飞牛影视接口签名参数。三项均来自影视 0.9.8 自带 web 前端的打包代码，
// 属于对端约定值，本应用无法自行生成或轮换：
//   - fnTVAppSecret：影视校验请求签名时使用的应用密钥；
//   - fnTVSignatureSaltEncoded：前端混淆形态的盐值，按字节与 fnTVSignatureSaltMask 异或还原；
//   - fnTVClientVersion：影视 web 客户端版本号，随版本变化，部分接口会据此判断兼容性。
//
// 风险提示：以上值随影视版本升级可能变化，一旦对端改版，签名校验会立即失败，
// 表现为"影视接口 HTTP 401/403"。集中收敛在此处，便于单点替换；请勿在其它位置散落硬编码。
const (
	fnTVAppSecret         = "NDzZTVxnRKP8Z0jXg1VAMonaG8akvh"
	fnTVClientVersion     = "631"
	fnTVSignatureSaltMask = 169
)

// fnTVSignatureSaltEncoded 是还原前的混淆盐值，仅在包初始化时读取一次。
var fnTVSignatureSaltEncoded = []byte{152, 159, 234, 234, 236, 235, 154, 237, 132, 232, 235, 157, 155, 132, 153, 158, 158, 237, 132, 154, 159, 232, 152, 132, 239, 154, 156, 156, 154, 155, 157, 236, 157, 155, 154, 158}

// fnTVSignatureSalt 是异或还原后的签名字节串，首次使用时计算并复用，
// 避免每次请求都分配新切片。
var fnTVSignatureSalt = func() string {
	salt := make([]byte, len(fnTVSignatureSaltEncoded))
	for i, b := range fnTVSignatureSaltEncoded {
		salt[i] = b ^ fnTVSignatureSaltMask
	}
	return string(salt)
}()

type FnTV struct {
	Base  string
	Token string
	HTTP  *http.Client
}
type object = map[string]any

func str(o object, key string) string { v, _ := o[key].(string); return v }
func num(o object, key string) int {
	switch v := o[key].(type) {
	case float64:
		return int(v)
	case string:
		n, _ := strconv.Atoi(v)
		return n
	}
	return -1
}
func obj(v any) object { o, _ := v.(map[string]any); return o }
func list(v any) []object {
	a, ok := v.([]any)
	if !ok {
		return nil
	}
	out := []object{}
	for _, x := range a {
		if o := obj(x); o != nil {
			out = append(out, o)
		}
	}
	return out
}
func md5hex(s string) string { h := md5.Sum([]byte(s)); return hex.EncodeToString(h[:]) }
func newFnTV(base string) (*FnTV, error) {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("影视地址格式无效")
	}
	if u.Path != "" && u.Path != "/v" {
		return nil, errors.New("请输入 NAS 地址或影视 /v 地址")
	}
	// API paths already include /v.
	u.Path = ""
	return &FnTV{Base: u.String(), HTTP: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (f *FnTV) Call(ctx context.Context, method, path string, body any) (any, error) {
	var raw []byte
	var err error
	if body != nil {
		raw, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, f.Base+path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	// 签名算法与对端 0.9.8 web 前端保持一致，参数集中在文件头部常量中。
	nonce := strconv.FormatInt(time.Now().UnixNano()%900000+100000, 10)
	stamp := strconv.FormatInt(time.Now().UnixMilli(), 10)
	content := string(raw)
	if method == "GET" {
		content = req.URL.Query().Encode()
		content = strings.ReplaceAll(content, "+", "%20")
		if decoded, e := url.QueryUnescape(content); e == nil {
			content = decoded
		}
	}
	sig := md5hex(strings.Join([]string{fnTVAppSecret, req.URL.Path, nonce, stamp, md5hex(content), fnTVSignatureSalt}, "_"))
	req.Header.Set("Authx", "nonce="+nonce+"&timestamp="+stamp+"&sign="+sig)
	req.Header.Set("Authorization", f.Token)
	req.Header.Set("X-Trim-Client", "web")
	req.Header.Set("X-Trim-Client-Version", fnTVClientVersion)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return nil, errors.New("无法连接影视服务，请检查地址和网络")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("影视接口 HTTP %d", resp.StatusCode)
	}
	var envelope struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data any    `json:"data"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&envelope); err != nil {
		return nil, errors.New("影视接口返回格式不兼容")
	}
	if envelope.Code != 0 {
		return nil, fmt.Errorf("影视接口错误 %d：%s", envelope.Code, envelope.Msg)
	}
	return envelope.Data, nil
}

// 说明：原 FnTV.Login（用户名 + 密码直连影视换取 token）从未被任何请求路径调用，
// 实际授权统一走 NAS OAuth（见 oauthStart / oauthCallback），故已删除，避免留存
// 一条绕过 NAS 身份的登录通道。
func (f *FnTV) Libraries(ctx context.Context) ([]object, error) {
	data, err := f.Call(ctx, "GET", "/v/api/v1/mdb/list", nil)
	if err != nil {
		return nil, err
	}
	result := list(data)
	if len(result) == 0 {
		if o := obj(data); o != nil {
			result = list(o["list"])
		}
	}
	if result == nil {
		return nil, errors.New("媒体库列表格式不兼容")
	}
	return result, nil
}
func recognized(o object) bool {
	return str(o, "trim_id") != "" || str(o, "imdb_id") != "" || str(o, "tmdb_id") != "" || num(o, "recognition_status") == 1
}
func yearOf(o object) string {
	if y := str(o, "release_date"); len(y) >= 4 && yearPattern.MatchString(y[:4]) {
		return y[:4]
	}
	if y := str(o, "year"); yearPattern.MatchString(y) {
		return y
	}
	if n := num(o, "year"); n > 0 {
		return strconv.Itoa(n)
	}
	return ""
}
func (f *FnTV) Scan(ctx context.Context, guid string, progress func(string)) ([]Media, []string, error) {
	out := []Media{}
	warnings := []string{}
	seen := map[string]bool{}
	for page := 1; page <= 1000; page++ {
		progress(fmt.Sprintf("正在读取第 %d 页", page))
		data, err := f.Call(ctx, "POST", "/v/api/v1/item/list", object{"ancestor_guid": guid, "tags": object{"type": []string{"Movie", "TV"}}, "exclude_grouped_video": 1, "sort_type": "ASC", "sort_column": "title", "page": page, "page_size": 100})
		if err != nil {
			return nil, nil, err
		}
		o := obj(data)
		items := list(o["list"])
		if o == nil || items == nil {
			return nil, nil, errors.New("条目列表格式不兼容")
		}
		added := 0
		for _, item := range items {
			id := str(item, "guid")
			kind := str(item, "type")
			title := str(item, "title")
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			added++
			if kind != "Movie" && kind != "TV" {
				continue
			}
			if !recognized(item) {
				warnings = append(warnings, title+"：没有识别标识，跳过")
				continue
			}
			progress("正在读取文件：" + title)
			// Item detail supplies corrected title/year where the list is incomplete.
			detail, err := f.Call(ctx, "GET", "/v/api/v1/item/"+url.PathEscape(id), nil)
			if err != nil {
				return nil, nil, err
			}
			d := obj(detail)
			if d == nil {
				return nil, nil, errors.New("条目详情格式不兼容")
			}
			merged := object{}
			for k, v := range item {
				merged[k] = v
			}
			for k, v := range d {
				merged[k] = v
			}
			title = str(merged, "title")
			year := yearOf(merged)
			filesData, err := f.Call(ctx, "GET", "/v/api/v1/media/itemfile/"+url.PathEscape(id), nil)
			if err != nil {
				return nil, nil, err
			}
			files := list(filesData)
			if files == nil {
				return nil, nil, errors.New("文件列表格式不兼容")
			}
			if len(files) == 0 {
				warnings = append(warnings, title+"：未返回本地文件")
			}
			episodeTitles := map[string]string{}
			if kind == "TV" && len(files) > 0 {
				seasonsData, seasonErr := f.Call(ctx, "GET", "/v/api/v1/season/list/"+url.PathEscape(id), nil)
				if seasonErr != nil {
					return nil, nil, seasonErr
				}
				seasons := list(seasonsData)
				if seasons == nil {
					return nil, nil, errors.New("季识别结果格式不兼容")
				}
				for _, season := range seasons {
					seasonID := str(season, "guid")
					if seasonID == "" {
						return nil, nil, errors.New("季识别结果缺少标识")
					}
					episodesData, episodeErr := f.Call(ctx, "GET", "/v/api/v1/episode/list/"+url.PathEscape(seasonID), nil)
					if episodeErr != nil {
						return nil, nil, episodeErr
					}
					episodes := list(episodesData)
					if episodes == nil {
						return nil, nil, errors.New("单集识别结果格式不兼容")
					}
					for _, ep := range episodes {
						seasonNumber := num(ep, "season_number")
						if _, present := ep["season_number"]; !present {
							seasonNumber = num(season, "season_number")
						}
						key := fmt.Sprintf("%d:%d", seasonNumber, num(ep, "episode_number"))
						episodeTitles[key] = str(ep, "title")
					}
				}
			}
			for _, file := range files {
				path := str(file, "file_path")
				if path == "" {
					path = str(file, "path")
				}
				if path == "" {
					warnings = append(warnings, title+"：文件缺少路径，跳过")
					continue
				}
				m := Media{Poster: str(merged, "poster"), ID: str(file, "media_guid"), Title: title, Year: year, Kind: "movie", Source: path, Season: num(file, "season_number"), Episode: num(file, "episode_number")}
				if m.ID == "" {
					m.ID = id + ":" + path
				}
				if kind == "TV" {
					m.Kind = "episode"
					m.EpisodeTitle = episodeTitles[fmt.Sprintf("%d:%d", m.Season, m.Episode)]
				}
				out = append(out, m)
			}
		}
		if added == 0 || (num(o, "total") >= 0 && page*100 >= num(o, "total")) || len(items) < 100 {
			break
		}
		if page == 1000 {
			return nil, nil, errors.New("媒体库超过扫描上限")
		}
	}
	return out, warnings, nil
}

func (f *FnTV) LibraryDirectories(ctx context.Context, guid string) ([]string, error) {
	data, e := f.Call(ctx, "GET", "/v/api/v1/mdb/"+url.PathEscape(guid), nil)
	if e != nil {
		return nil, e
	}
	raw, ok := obj(data)["dir_list"].([]any)
	if !ok || len(raw) == 0 {
		return nil, errors.New("影视库未返回本地目录，无法整理")
	}
	dirs := []string{}
	for _, v := range raw {
		p, ok := v.(string)
		if !ok || p == "" {
			return nil, errors.New("影视库目录格式不兼容")
		}
		dirs = append(dirs, p)
	}
	return dirs, nil
}
