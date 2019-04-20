package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"time"

	"github.com/go-redis/redis"
	"github.com/gorilla/mux"
	"gopkg.in/yaml.v2"
)

var redisClient *redis.Client
var internalRegistries []string
var externalRegistries []string

func cachelessProxy(wr http.ResponseWriter, r *http.Request) {
	log.Printf("A cachless request handling for %s", r.URL.Path)

	var responseError error

	for _, internalRegistryURL := range internalRegistries {
		proxiedURL := fmt.Sprintf("%s%s", internalRegistryURL, r.URL.Path)

		client := &http.Client{}
		req, _ := http.NewRequest(r.Method, proxiedURL, r.Body)
		for name, value := range r.Header {
			req.Header.Set(name, value[0])
		}
		resp, responseError := client.Do(req)
		r.Body.Close()

		if responseError == nil {
			log.Printf("Internal registry %s responded to %s request of %s", internalRegistryURL, r.Method, r.URL.Path)

			for k, v := range resp.Header {
				wr.Header().Set(k, v[0])
			}
			wr.WriteHeader(resp.StatusCode)
			io.Copy(wr, resp.Body)
			resp.Body.Close()
			return
		}
	}

	log.Printf("All internal registries failed to response to %s %s with no errors", r.Method, r.URL.Path)
	http.Error(wr, responseError.Error(), http.StatusInternalServerError)
	return
}

func cachedProxy(wr http.ResponseWriter, r *http.Request, cachingPeriod time.Duration) {

	npmResponse, err := redisClient.HGetAll(r.URL.Path).Result()
	if err == redis.Nil || err != nil || len(npmResponse) == 0 {
		var responseError error

		for _, internalRegistryURL := range internalRegistries {
			proxiedURL := fmt.Sprintf("%s%s", internalRegistryURL, r.URL.Path)

			client := &http.Client{}
			req, _ := http.NewRequest(r.Method, proxiedURL, r.Body)
			for name, value := range r.Header {
				req.Header.Set(name, value[0])
			}
			resp, responseError := client.Do(req)
			r.Body.Close()

			if responseError == nil && resp.StatusCode == http.StatusOK {
				log.Printf("Internal registry %s responded to %s request of %s", internalRegistryURL, r.Method, r.URL.Path)

				for k, v := range resp.Header {
					wr.Header().Set(k, v[0])
				}
				wr.WriteHeader(resp.StatusCode)
				bytesBody, _ := httputil.DumpResponse(resp, true)
				io.Copy(wr, resp.Body)
				resp.Body.Close()

				writePackageInfo(r.URL.Path, resp, string(bytesBody), cachingPeriod)
				return
			}
		}

		for _, externalRegistryURL := range externalRegistries {
			proxiedURL := fmt.Sprintf("%s%s", externalRegistryURL, r.URL.Path)

			client := &http.Client{}
			req, _ := http.NewRequest(r.Method, proxiedURL, r.Body)
			for name, value := range r.Header {
				req.Header.Set(name, value[0])
			}
			resp, responseError := client.Do(req)
			r.Body.Close()

			if responseError == nil {
				log.Printf("External registry %s responded to %s request of %s", externalRegistryURL, r.Method, r.URL.Path)

				for k, v := range resp.Header {
					wr.Header().Set(k, v[0])
				}
				wr.WriteHeader(resp.StatusCode)
				bytesBody, _ := httputil.DumpResponse(resp, true)
				io.Copy(wr, resp.Body)
				resp.Body.Close()

				writePackageInfo(r.URL.Path, resp, string(bytesBody), cachingPeriod)
				return
			}
		}

		log.Printf("All internal registries failed to response to %s %s with no errors", r.Method, r.URL.Path)
		http.Error(wr, responseError.Error(), http.StatusInternalServerError)
	} else {
		if npmResponse["Etag"] == r.Header.Get("If-None-Match") {
			log.Printf("Found the tag")
			wr.Header().Set("Etag", npmResponse["Etag"])
			wr.WriteHeader(304)
		} else {
			log.Printf("Found tag but it is now different")
			responseBuffer := bufio.NewReader(bytes.NewReader([]byte(npmResponse["wholeResponse"])))

			resp, _ := http.ReadResponse(responseBuffer, r)

			for k, v := range resp.Header {
				wr.Header().Set(k, v[0])
			}
			wr.WriteHeader(resp.StatusCode)

			io.Copy(wr, resp.Body)

			resp.Body.Close()
		}
	}
}

func getPackageEtag(packageURL string, requestEtag string) bool {
	packageEtag := fmt.Sprintf("%s/Etag", packageURL)
	log.Printf("Looking for %s", packageEtag)

	cachedEtag, err := redisClient.Get(packageEtag).Result()
	if err == redis.Nil {
		return false
	} else if err != nil {
		return false
	} else {
		log.Printf("Cached etag is %s", cachedEtag)
		return cachedEtag == requestEtag
	}
}

func writePackageInfo(packageURL string, npmRegisteryResponse *http.Response, npmRegisteryBody string, cachingPeriod time.Duration) {
	switch npmRegisteryResponse.StatusCode {
	case 200:
		npmResponse := make(map[string]interface{})

		npmResponse["Etag"] = npmRegisteryResponse.Header.Get("Etag")
		npmResponse["wholeResponse"] = npmRegisteryBody
		redisClient.HMSet(packageURL, npmResponse)
	case 304:
		redisClient.HSet(packageURL, "Etag", npmRegisteryResponse.Header.Get("Etag"))
	}

	if cachingPeriod > -1 {
		redisClient.Expire(packageURL, cachingPeriod)
	}
}

func longTermCachfulProxy(wr http.ResponseWriter, r *http.Request) {
	log.Printf("A long term cached request handling for %s", r.URL.Path)

	cachedProxy(wr, r, -1)
}

func shortTermCachfulProxy(wr http.ResponseWriter, r *http.Request) {
	log.Printf("A short term cached request handling for %s", r.URL.Path)

	cachedProxy(wr, r, 24*time.Hour)
}

func leveeRouter() *mux.Router {
	router := mux.NewRouter()

	router.HandleFunc("/npm", shortTermCachfulProxy).Methods("GET")
	router.HandleFunc("/{package}", longTermCachfulProxy).Methods("GET")
	router.HandleFunc("/{package}/{version}", longTermCachfulProxy).Methods("GET")
	router.HandleFunc("/", cachelessProxy)

	return router
}

type Config struct {
	LeveePort string `yaml:"leveePort"`
	Redis     struct {
		Address  string `yaml:"address"`
		Password string `yaml:"password"`
		DB       int    `yaml:"db"`
	} `yaml:redis`
	InternalRegistries []string `yaml:"internalRegistries"`
	ExternalRegistries []string `yaml:"externalRegistries"`
}

func main() {
	filename, _ := filepath.Abs(os.Args[1])
	yamlFile, err := ioutil.ReadFile(filename)

	if err != nil {
		panic(err)
	}

	var config Config

	err = yaml.Unmarshal(yamlFile, &config)
	if err != nil {
		panic(err)
	}

	listeningPort := fmt.Sprintf(":%s", config.LeveePort)
	log.Printf("Welcome to the leeve")
	log.Printf("Listens on the port of the year the song was published in %s", listeningPort)

	redisClient = redis.NewClient(&redis.Options{
		Addr:     config.Redis.Address,
		Password: config.Redis.Password,
		DB:       config.Redis.DB,
	})
	internalRegistries = config.InternalRegistries
	externalRegistries = config.ExternalRegistries

	router := leveeRouter()
	log.Fatal(http.ListenAndServe(listeningPort, router))
}
