/*
Copyright 2022.

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

package controllers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	v1core "k8s.io/api/core/v1"
	v1meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/client-go/tools/record"
	"net"
	"net/http"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	dyndnsv1alpha1 "contrib.strato.com/strato-dyndns/api/v1alpha1"
)

const (
	externalIpRawUrl         string = "https://myexternalip.com/raw"
	stratoUpdateDnsUrl       string = "https://%s:%s@dyndns.strato.com/nic/update?hostname=%s&myip=%s"
	defaultIntervalInMinutes int32  = 5
)

// DomainReconciler reconciles a Domain object
type DomainReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

//+kubebuilder:rbac:groups=dyndns.contrib.strato.com,resources=domains,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=dyndns.contrib.strato.com,resources=domains/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=dyndns.contrib.strato.com,resources=domains/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Domain object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.12.2/pkg/reconcile
func (r *DomainReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.Log.V(0).WithValues("namespace", req.Namespace, "name", req.Name)
	var success bool = true
	var mode Mode = Dynamic

	logger.V(10).Info("starting dyndns update")

	var domain dyndnsv1alpha1.Domain
	if err := r.Get(ctx, req.NamespacedName, &domain); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Error(err, "finding Domain failed")
			return ctrl.Result{}, nil
		}

		logger.Error(err, "fetching Domain failed")
		return ctrl.Result{}, err
	}

	instance := domain.DeepCopyObject()
	wasSuccess := r.wasLastLastReconciliationSuccessful(&domain)
	logger = logger.WithValues("fqdn", domain.Spec.Fqdn)

	// break reconciliation loop if is not enabled
	if !domain.Spec.Enabled {
		return ctrl.Result{}, nil
	}

	// define interval between reconciliation loops
	interval := defaultIntervalInMinutes
	if domain.Spec.IntervalInMinutes != nil {
		interval = *domain.Spec.IntervalInMinutes
	}

	// change mode to manual in presence of an explicit ip address in specs
	if domain.Spec.IpAddress != nil {
		mode = Manual
	}

	// is reconciliation loop started too soon because of an external event?
	if domain.Status.LastReconciliationLoop != nil && mode == Dynamic { //domain.Spec.IpAddress == nil {
		if time.Since(domain.Status.LastReconciliationLoop.Time) < (time.Minute*time.Duration(interval)) && wasSuccess {
			sinceLastRunDuration := time.Since(domain.Status.LastReconciliationLoop.Time)
			intervalDuration := time.Minute * time.Duration(interval)
			requeueAfter := intervalDuration - sinceLastRunDuration

			logger.Info("skipped turn", "sinceLastRun", sinceLastRunDuration, "requeueAfter", requeueAfter)
			return ctrl.Result{RequeueAfter: time.Until(time.Now().Add(requeueAfter))}, nil
		}
	}

	domainCopy := *domain.DeepCopy()
	currentIpAddress := domain.Status.IpAddress
	var newIpAddress *string

	switch mode {
	case Dynamic:
		externalIpAddress, err := r.getExternalIpAddress()
		if err != nil {
			logger.Error(err, "retrieving external ip failed")
			r.Recorder.Eventf(instance, v1core.EventTypeWarning, "RetrieveExternalIpFailed", err.Error())

			success = false
		} else {
			newIpAddress = externalIpAddress
		}
	case Manual:
		newIpAddress = domain.Spec.IpAddress
	}

	// proceed to update Strato DynDNS only if a valid IP address was found
	if newIpAddress != nil {
		// if last reconciliation loop was successful and there is no ip change skip the loop
		if *newIpAddress == currentIpAddress && wasSuccess {
			logger.Info("updating dyndns skipped, ip is up-to-date", "ipAddress", currentIpAddress, "mode", mode.String())
			r.Recorder.Event(instance, v1core.EventTypeNormal, "DynDnsUpdateSkipped", "updating skipped, ip is up-to-date")
		} else {
			logger.Info("updating dyndns", "ipAddress", newIpAddress, "mode", mode.String())

			passwordRef := domain.Spec.Password
			objectKey := client.ObjectKey{
				Namespace: req.Namespace,
				Name:      passwordRef.Name,
			}

			var secret v1core.Secret
			if err := r.Get(ctx, objectKey, &secret); err != nil {
				if apierrors.IsNotFound(err) {
					logger.Error(err, "finding Secret failed")
					return ctrl.Result{}, nil
				}

				logger.Error(err, "fetching Secret failed")
				return ctrl.Result{}, err
			}

			password := string(secret.Data["password"])
			if err := r.updateDns(domain.Spec.Fqdn, domain.Spec.Fqdn, password, *newIpAddress); err != nil {
				logger.Error(err, "updating dyndns failed")
				r.Recorder.Eventf(instance, v1core.EventTypeWarning, "DynDnsUpdateFailed", err.Error())

				success = false
			} else {
				logger.Info("updating dyndns completed")
				r.Recorder.Eventf(instance, v1core.EventTypeNormal, "DynDnsUpdateCompleted", "updating dyndns completed")

				success = true
			}
		}
	}

	// update the status of the CR no matter what, but assign a new IP address in the status
	// only when Strato DynDNS update was successful
	if success {
		domainCopy.Status.IpAddress = *newIpAddress
	}

	domainCopy.Status.LastReconciliationLoop = &v1meta.Time{Time: time.Now()}
	domainCopy.Status.LastReconciliationResult = &success
	domainCopy.Status.Enabled = domain.Spec.Enabled
	domainCopy.Status.Mode = mode.String()

	// update the status of the CR
	if err := r.Status().Update(ctx, &domainCopy); err != nil {
		logger.Error(err, "updating status failed") //

		requeueAfterUpdateStatusFailure := time.Now().Add(time.Second * time.Duration(15))
		return ctrl.Result{RequeueAfter: time.Until(requeueAfterUpdateStatusFailure)}, err
	}

	// if Mode is Manual and we updated DynDNS with success, then we don't requeue and we will rely only on
	// events that will be triggered externally from YAML updates of the CR
	if mode == Manual && success {
		return ctrl.Result{}, nil
	}

	requeueAfter := time.Now().Add(time.Minute * time.Duration(interval))

	logger.Info("requeue", "nextRun", fmt.Sprintf("%s", requeueAfter.Local().Format(time.RFC822)))
	logger.V(10).Info("finished dyndns update")

	return ctrl.Result{RequeueAfter: time.Until(requeueAfter)}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *DomainReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&dyndnsv1alpha1.Domain{}).
		Complete(r)
}

func (r *DomainReconciler) getExternalIpAddress() (*string, error) {
	resp, err := http.Get(externalIpRawUrl)
	if err != nil {
		return nil, err
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
		}
	}(resp.Body)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	ipAddress := strings.TrimSpace(string(body))

	if ipAddress == "" || net.ParseIP(ipAddress) == nil {
		return nil, errors.New("no valid ip address found")
	}

	return &ipAddress, nil
}

func (r *DomainReconciler) updateDns(domain, username, password, ipAddress string) error {
	url := fmt.Sprintf(stratoUpdateDnsUrl, username, password, domain, ipAddress)
	method := "GET"
	httpClient := &http.Client{}

	request, err := http.NewRequest(method, url, nil)
	if err != nil {
		return err
	}

	response, err := httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	body, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return err
	}

	if !strings.Contains(string(body), "good") && !strings.Contains(string(body), "nochg") {
		return errors.New(string(body))
	}

	return nil
}

func (r *DomainReconciler) wasLastLastReconciliationSuccessful(domain *dyndnsv1alpha1.Domain) bool {
	return domain.Status.LastReconciliationResult != nil && *domain.Status.LastReconciliationResult == true
}

type Mode int

const (
	Manual Mode = iota
	Dynamic
)

func (m Mode) String() string {
	var values []string = []string{"Manual", "Dynamic"}
	name := values[m]

	return name
}
