package main

import (
	"fmt"
	"github.com/Sirupsen/logrus"
	"github.com/fsouza/go-dockerclient"
	"strings"
	"sync"
	"time"
)

const PingInterval = 10 * time.Second
const ReconnectTime = 10 * time.Second

type RoutesHandleFunc func(routes Routes)

func createRoutes(client *docker.Client) (routes Routes, err error) {
	opts := docker.ListContainersOptions{}
	containers, err := client.ListContainers(opts)
	if err != nil {
		return
	}

	wg := sync.WaitGroup{}
	ch := make(chan *docker.Container)

	for _, container := range containers {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			container, err := client.InspectContainer(id)
			if err != nil {
				logrus.WithField("id", id).WithError(err).Errorln("Failed inspecing container")
				return
			}
			ch <- container
		}(container.ID)
	}

	go func() {
		wg.Wait()
		close(ch)
	}()

	routes = make(Routes)

	for container := range ch {
		route := NewRouteBuilder()
		route.ParseAll(container.Config.Env...)

		// Try to find first suitable port if not specified from list of ports
		if route.Upstream.Port == "" {
			for _, port := range strings.Split(*ports, ",") {
				portDef := fmt.Sprintf("%s/tcp", port)
				if _, ok := container.NetworkSettings.Ports[docker.Port(portDef)]; ok {
					route.Upstream.Port = port
					break
				}
			}
		}

		// Fail if we can't find a port
		if route.Upstream.Port == "" {
			logrus.WithField("name", container.Name).WithField("id", container.ID[0:7]).
				Debugln("Couldn't find a port to expose...")
			continue
		}

		route.Upstream.Container = container.Name

		// Try to find bindings for specified ports
		portDef := fmt.Sprintf("%s/tcp", route.Upstream.Port)
		bindings := container.NetworkSettings.Ports[docker.Port(portDef)]

		// Try to use bindings in order to access host (useful for Swarm nodes)
		for _, binding := range bindings {
			if binding.HostIP != "0.0.0.0" {
				route.Upstream.IP = binding.HostIP
				route.Upstream.Port = binding.HostPort
				break
			}
		}

		// Try to use address when connected to local bridge
		if container.Node == nil && route.Upstream.IP == "" {
			// This address make sense only when accessing locally
			route.Upstream.IP = container.NetworkSettings.IPAddress
		}

		// Try to use address when connected to other network
		if container.Node == nil && route.Upstream.IP == "" {
			for _, network := range container.NetworkSettings.Networks {
				if network.IPAddress != "" {
					route.Upstream.IP = network.IPAddress
					break
				}
			}
		}

		if route.Upstream.IP == "" {
			logrus.WithField("name", container.Name).WithField("id", container.ID[0:7]).
				Debugln("Couldn't find an IP to access container...")
			continue
		}

		if !route.isValid() {
			continue
		}

		logrus.WithField("name", container.Name).WithField("id", container.ID[0:7]).WithField("route", route).
			Debugln("Adding route...")
		routes.Add(route)
	}

	return
}

func watchEvents(updateFunc RoutesHandleFunc) {
	var client *docker.Client
	var err error
	var routes Routes

	for {
		if client == nil || client.Ping() == nil {
			client, err = docker.NewClientFromEnv()
			if err != nil {
				logrus.Errorln("Unable to connect to docker daemon:", err)
				time.Sleep(ReconnectTime)
				continue
			}

			logrus.Debugln("Connected to docker daemon...")
			routes, err = createRoutes(client)
			if err != nil {
				logrus.Errorln("Error enumerating routes:", err)
			}
			if err == nil && updateFunc != nil {
				updateFunc(routes)
			}
		}

		eventChan := make(chan *docker.APIEvents, 100)
		defer close(eventChan)

		watching := false
		for {
			if client == nil {
				break
			}
			err := client.Ping()
			if err != nil {
				logrus.Errorln("Unable to ping docker daemon:", err)
				if watching {
					client.RemoveEventListener(eventChan)
					watching = false
					client = nil
				}
				time.Sleep(ReconnectTime)
				break
			}

			if !watching {
				err = client.AddEventListener(eventChan)
				if err != nil && err != docker.ErrListenerAlreadyExists {
					logrus.Errorln("Error registering docker event listener:", err)
					time.Sleep(ReconnectTime)
					continue
				}
				watching = true
				logrus.Infoln("Watching docker events...")
			}

			select {
			case event := <-eventChan:
				if event == nil {
					if watching {
						client.RemoveEventListener(eventChan)
						watching = false
						client = nil
					}
					break
				}

				if event.Status == "start" || event.Status == "stop" || event.Status == "die" {
					logrus.Debugln("Received event", event.Status, "for container", event.ID[:12])
					routes, err = createRoutes(client)
					if err != nil {
						logrus.Errorln("Error enumerating routes:", err)
					}
					if err == nil && updateFunc != nil {
						updateFunc(routes)
					}
				}
			case <-time.After(PingInterval):
				// check for docker liveness
			}
		}
	}
}
