/*
 Copyright 2020 Qiniu Cloud (qiniu.com)

 Licensed under the Apache License, Version 2.0 (the "License");
 you may not use this file except in compliance with the License.
 You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

 Unless required by applicable law or agreed to in writing, software
 distributed under the License is distributed on an "AS IS" BASIS,
 WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 See the License for the specific language governing permissions and
 limitations under the License.
*/

package cover

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"golang.org/x/tools/cover"
	"k8s.io/test-infra/gopherage/pkg/cov"
)

// DefaultStore implements the IPersistence interface
var DefaultStore Store

// LogFile a file to save log.
const LogFile = "goc.log"

func init() {
	DefaultStore = NewFileStore()
}

// Run starts coverage host center
func Run(port string) {
	f, err := os.Create(LogFile)
	if err != nil {
		log.Fatalf("failed to create log file %s, err: %v", LogFile, err)
	}

	// both log to stdout and file by default
	mw := io.MultiWriter(f, os.Stdout)
	r := GocServer(mw)
	log.Fatal(r.Run(port))
}

// GocServer init goc server engine
func GocServer(w io.Writer) *gin.Engine {
	if w != nil {
		gin.DefaultWriter = w
	}
	r := gin.Default()
	// api to show the registered services
	r.StaticFile(PersistenceFile, "./"+PersistenceFile)

	v1 := r.Group("/v1")
	{
		v1.POST("/cover/register", registerService)
		v1.GET("/cover/profile", profile)
		v1.POST("/cover/clear", clear)
		v1.POST("/cover/init", initSystem)
		v1.GET("/cover/list", listServices)
	}

	return r
}

// Service is a entry under being tested
type Service struct {
	Name    string `form:"name" json:"name" binding:"required"`
	Address string `form:"address" json:"address" binding:"required"`
}

//listServices list all the registered services
func listServices(c *gin.Context) {
	services := DefaultStore.GetAll()
	c.JSON(http.StatusOK, services)
}

func registerService(c *gin.Context) {
	var service Service
	if err := c.ShouldBind(&service); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	u, err := url.Parse(service.Address)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	realIP := c.ClientIP()
	if host != realIP {
		log.Printf("the registed host %s of service %s is different with the real one %s, here we choose the real one", service.Name, host, realIP)
		service.Address = fmt.Sprintf("http://%s:%s", realIP, port)
	}
	if err := DefaultStore.Add(service); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"result": "success"})
}

func profile(c *gin.Context) {
	svrsUnderTest := DefaultStore.GetAll()
	var mergedProfiles = make([][]*cover.Profile, len(svrsUnderTest))
	for _, addrs := range svrsUnderTest {
		for _, addr := range addrs {
			pp, err := NewWorker(addr).Profile()
			if err != nil {
				c.JSON(http.StatusExpectationFailed, gin.H{"error": err.Error()})
				return
			}
			profile, err := convertProfile(pp)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			mergedProfiles = append(mergedProfiles, profile)
		}
	}

	if len(mergedProfiles) == 0 {
		c.JSON(http.StatusOK, "no profiles")
		return
	}

	merged, err := cov.MergeMultipleProfiles(mergedProfiles)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if err := cov.DumpProfile(merged, c.Writer); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
}

func clear(c *gin.Context) {
	svrsUnderTest := DefaultStore.GetAll()
	for svc, addrs := range svrsUnderTest {
		for _, addr := range addrs {
			pp, err := NewWorker(addr).Clear()
			if err != nil {
				c.JSON(http.StatusExpectationFailed, gin.H{"error": err.Error()})
				return
			}
			fmt.Fprintf(c.Writer, "Register service %s: %s coverage counter %s", svc, addr, string(pp))
		}
	}
}

func initSystem(c *gin.Context) {
	if err := DefaultStore.Init(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, "")
}

func convertProfile(p []byte) ([]*cover.Profile, error) {
	// Annoyingly, ParseProfiles only accepts a filename, so we have to write the bytes to disk
	// so it can read them back.
	// We could probably also just give it /dev/stdin, but that'll break on Windows.
	tf, err := ioutil.TempFile("", "")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp file, err: %v", err)
	}
	defer tf.Close()
	defer os.Remove(tf.Name())
	if _, err := io.Copy(tf, bytes.NewReader(p)); err != nil {
		return nil, fmt.Errorf("failed to copy data to temp file, err: %v", err)
	}

	return cover.ParseProfiles(tf.Name())
}
