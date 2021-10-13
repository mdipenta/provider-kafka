/*
Copyright 2020 The Crossplane Authors.

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

package topic

import (
	"context"

	"github.com/pkg/errors"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl/plain"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"

	v1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	"github.com/crossplane/crossplane-runtime/pkg/event"
	"github.com/crossplane/crossplane-runtime/pkg/logging"
	"github.com/crossplane/crossplane-runtime/pkg/meta"
	"github.com/crossplane/crossplane-runtime/pkg/ratelimiter"
	"github.com/crossplane/crossplane-runtime/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/pkg/resource"

	"github.com/crossplane-contrib/provider-kafka/apis/topic/v1alpha1"
	apisv1alpha1 "github.com/crossplane-contrib/provider-kafka/apis/v1alpha1"
)

const (
	errNotTopic     = "managed resource is not a Topic custom resource"
	errTrackPCUsage = "cannot track ProviderConfig usage"
	errGetPC        = "cannot get ProviderConfig"
	errGetCreds     = "cannot get credentials"

	errNewClient = "cannot create new Kafka client"
)

func newKafkaClient(data []byte) (*kadm.Client, error) {
	kc := KafkaConfig{}

	if err := json.Unmarshal(data, &kc); err != nil {
		return nil, errors.Wrap(err, "cannot parse credentials")
	}

	opts := []kgo.Opt{
		kgo.SeedBrokers(kc.Brokers...),
	}

	if kc.SASL != nil {
		opts = append(opts, kgo.SASL(plain.Auth{
			User: kc.SASL.Username,
			Pass: kc.SASL.Password,
		}.AsMechanism()))
	}
	c, err := kgo.NewClient(opts...)
	if err != nil {
		return nil, err
	}
	return kadm.NewClient(c), nil
}

type KafkaConfig struct {
	Brokers []string   `json:"brokers"`
	SASL    *KafkaSASL `json:"sasl,omitempty"`
}

type KafkaSASL struct {
	Mechanism string `json:"mechanism"`
	Username  string `json:"username"`
	Password  string `json:"password"`
}

// Setup adds a controller that reconciles Topic managed resources.
func Setup(mgr ctrl.Manager, l logging.Logger, rl workqueue.RateLimiter) error {
	name := managed.ControllerName(v1alpha1.TopicGroupKind)

	o := controller.Options{
		RateLimiter: ratelimiter.NewDefaultManagedRateLimiter(rl),
	}

	r := managed.NewReconciler(mgr,
		resource.ManagedKind(v1alpha1.TopicGroupVersionKind),
		managed.WithExternalConnecter(&connector{
			kube:         mgr.GetClient(),
			usage:        resource.NewProviderConfigUsageTracker(mgr.GetClient(), &apisv1alpha1.ProviderConfigUsage{}),
			log:          l,
			newServiceFn: newKafkaClient}),
		managed.WithLogger(l.WithValues("controller", name)),
		managed.WithRecorder(event.NewAPIRecorder(mgr.GetEventRecorderFor(name))))

	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		WithOptions(o).
		For(&v1alpha1.Topic{}).
		Complete(r)
}

// A connector is expected to produce an ExternalClient when its Connect method
// is called.
type connector struct {
	kube         client.Client
	usage        resource.Tracker
	log          logging.Logger
	newServiceFn func(creds []byte) (*kadm.Client, error)
}

// Connect typically produces an ExternalClient by:
// 1. Tracking that the managed resource is using a ProviderConfig.
// 2. Getting the managed resource's ProviderConfig.
// 3. Getting the credentials specified by the ProviderConfig.
// 4. Using the credentials to form a client.
func (c *connector) Connect(ctx context.Context, mg resource.Managed) (managed.ExternalClient, error) {
	cr, ok := mg.(*v1alpha1.Topic)
	if !ok {
		return nil, errors.New(errNotTopic)
	}

	if err := c.usage.Track(ctx, mg); err != nil {
		return nil, errors.Wrap(err, errTrackPCUsage)
	}

	pc := &apisv1alpha1.ProviderConfig{}
	if err := c.kube.Get(ctx, types.NamespacedName{Name: cr.GetProviderConfigReference().Name}, pc); err != nil {
		return nil, errors.Wrap(err, errGetPC)
	}

	cd := pc.Spec.Credentials
	data, err := resource.CommonCredentialExtractor(ctx, cd.Source, c.kube, cd.CommonCredentialSelectors)
	if err != nil {
		return nil, errors.Wrap(err, errGetCreds)
	}

	svc, err := c.newServiceFn(data)
	if err != nil {
		return nil, errors.Wrap(err, errNewClient)
	}

	return &external{kafkaClient: svc, log: c.log}, nil
}

// An ExternalClient observes, then either creates, updates, or deletes an
// external resource to ensure it reflects the managed resource's desired state.
type external struct {
	kafkaClient *kadm.Client
	log         logging.Logger
}

func (c *external) Observe(ctx context.Context, mg resource.Managed) (managed.ExternalObservation, error) {
	cr, ok := mg.(*v1alpha1.Topic)
	if !ok {
		return managed.ExternalObservation{}, errors.New(errNotTopic)
	}

	td, err := c.kafkaClient.ListTopics(ctx, meta.GetExternalName(cr))
	if err != nil {
		return managed.ExternalObservation{}, err
	}

	t, ok := td[meta.GetExternalName(cr)]

	if !ok || errors.Is(t.Err, kerr.UnknownTopicOrPartition) {
		return managed.ExternalObservation{ResourceExists: false}, nil
	}

	cr.Status.AtProvider.ID = t.ID.String()
	cr.Status.SetConditions(v1.Available())

	upToDate := true
	if len(t.Partitions) != cr.Spec.ForProvider.Partitions {
		upToDate = false
	}
	if len(t.Partitions) > 0 && len(t.Partitions[0].Replicas) != cr.Spec.ForProvider.ReplicationFactor {
		upToDate = false
	}

	return managed.ExternalObservation{
		ResourceExists:   true,
		ResourceUpToDate: upToDate,
	}, nil
}

func (c *external) Create(ctx context.Context, mg resource.Managed) (managed.ExternalCreation, error) {
	cr, ok := mg.(*v1alpha1.Topic)
	if !ok {
		return managed.ExternalCreation{}, errors.New(errNotTopic)
	}

	resp, err := c.kafkaClient.CreateTopics(ctx, int32(cr.Spec.ForProvider.Partitions), int16(cr.Spec.ForProvider.ReplicationFactor), nil, meta.GetExternalName(cr))
	if err != nil {
		return managed.ExternalCreation{}, err
	}

	if len(resp) != 1 {
		return managed.ExternalCreation{}, errors.Errorf("unexpected number of createTopicResponse %d", len(resp))
	}
	if resp[0].Err != nil {
		return managed.ExternalCreation{}, errors.Wrap(resp[0].Err, "create failed")
	}

	cr.Status.AtProvider.ID = resp[0].ID.String()
	return managed.ExternalCreation{}, nil
}

func (c *external) Update(_ context.Context, _ resource.Managed) (managed.ExternalUpdate, error) {
	c.log.Info("topic updates not supported yet!")
	// todo(turkenh): Support topic updates
	return managed.ExternalUpdate{}, nil
}

func (c *external) Delete(ctx context.Context, mg resource.Managed) error {
	cr, ok := mg.(*v1alpha1.Topic)
	if !ok {
		return errors.New(errNotTopic)
	}

	resp, err := c.kafkaClient.DeleteTopics(ctx, meta.GetExternalName(cr))
	if err != nil {
		return err
	}
	if len(resp) != 1 {
		errors.Errorf("unexpected number of deleteTopicResponse %d", len(resp))
	}
	if resp[0].Err != nil {
		errors.Wrap(resp[0].Err, "delete failed")
	}

	return nil
}
