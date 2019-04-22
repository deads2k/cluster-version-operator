package internal

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"

	configv1 "github.com/openshift/api/config/v1"
	configclientv1 "github.com/openshift/client-go/config/clientset/versioned/typed/config/v1"

	"github.com/openshift/cluster-version-operator/lib"
	"github.com/openshift/cluster-version-operator/lib/resourcebuilder"
	"github.com/openshift/cluster-version-operator/pkg/payload"
)

var (
	osScheme = runtime.NewScheme()
	osCodecs = serializer.NewCodecFactory(osScheme)
)

func init() {
	if err := configv1.AddToScheme(osScheme); err != nil {
		panic(err)
	}
}

// readClusterOperatorV1OrDie reads clusteroperator object from bytes. Panics on error.
func readClusterOperatorV1OrDie(objBytes []byte) *configv1.ClusterOperator {
	requiredObj, err := runtime.Decode(osCodecs.UniversalDecoder(configv1.SchemeGroupVersion), objBytes)
	if err != nil {
		panic(err)
	}
	return requiredObj.(*configv1.ClusterOperator)
}

type clusterOperatorBuilder struct {
	client        ClusterOperatorsGetter
	eventRecorder record.EventRecorder

	raw      []byte
	modifier resourcebuilder.MetaV1ObjectModifierFunc
	mode     resourcebuilder.Mode
}

func newClusterOperatorBuilder(config *rest.Config, m lib.Manifest, eventRecorder record.EventRecorder) resourcebuilder.Interface {
	return NewClusterOperatorBuilder(clientClusterOperatorsGetter{
		getter: configclientv1.NewForConfigOrDie(config).ClusterOperators(),
	}, m, eventRecorder)
}

// ClusterOperatorsGetter abstracts object access with a client or a cache lister.
type ClusterOperatorsGetter interface {
	Get(name string) (*configv1.ClusterOperator, error)
}

type clientClusterOperatorsGetter struct {
	getter configclientv1.ClusterOperatorInterface
}

func (g clientClusterOperatorsGetter) Get(name string) (*configv1.ClusterOperator, error) {
	return g.getter.Get(name, metav1.GetOptions{})
}

// NewClusterOperatorBuilder accepts the ClusterOperatorsGetter interface which may be implemented by a
// client or a lister cache.
func NewClusterOperatorBuilder(client ClusterOperatorsGetter, m lib.Manifest, eventRecorder record.EventRecorder) resourcebuilder.Interface {
	return &clusterOperatorBuilder{
		client:        client,
		eventRecorder: eventRecorder,
		raw:           m.Raw,
	}
}

func (b *clusterOperatorBuilder) WithMode(m resourcebuilder.Mode) resourcebuilder.Interface {
	b.mode = m
	return b
}

func (b *clusterOperatorBuilder) WithModifier(f resourcebuilder.MetaV1ObjectModifierFunc) resourcebuilder.Interface {
	b.modifier = f
	return b
}

func (b *clusterOperatorBuilder) Do(ctx context.Context) error {
	os := readClusterOperatorV1OrDie(b.raw)
	if b.modifier != nil {
		b.modifier(os)
	}
	return waitForOperatorStatusToBeDone(ctx, 1*time.Second, b.client, os, b.mode, b.eventRecorder)
}

func waitForOperatorStatusToBeDone(ctx context.Context, interval time.Duration, client ClusterOperatorsGetter, expected *configv1.ClusterOperator, mode resourcebuilder.Mode, eventRecorder record.EventRecorder) error {
	// involvedObjectRef sets the namespace events go into
	involvedObjectRef := &corev1.ObjectReference{
		Namespace: "openshift-cluster-version",
		Name:      "cvo",
	}
	startTime := time.Now()

	// we emit the start event so that watching events tells a high level of story of what we're waiting for when.
	eventRecorder.Eventf(involvedObjectRef, corev1.EventTypeNormal, "ClusterOperatorWaitStarted", "start waiting for clusteroperator/%s", expected.Name)

	var lastErr error
	err := wait.PollImmediateUntil(interval, func() (bool, error) {
		actual, err := client.Get(expected.Name)
		if err != nil {
			lastErr = &payload.UpdateError{
				Nested:  err,
				Reason:  "ClusterOperatorNotAvailable",
				Message: fmt.Sprintf("Cluster operator %s has not yet reported success", expected.Name),
				Name:    expected.Name,
			}
			return false, nil
		}

		// undone is map of operand to tuple of (expected version, actual version)
		// for incomplete operands.
		undone := map[string][]string{}
		for _, expOp := range expected.Status.Versions {
			undone[expOp.Name] = []string{expOp.Version}
			for _, actOp := range actual.Status.Versions {
				if actOp.Name == expOp.Name {
					undone[expOp.Name] = append(undone[expOp.Name], actOp.Version)
					if actOp.Version == expOp.Version {
						delete(undone, expOp.Name)
					}
					break
				}
			}
		}
		if len(undone) > 0 {
			var keys []string
			for k := range undone {
				keys = append(keys, k)
			}
			sort.Strings(keys)

			var reports []string
			for _, op := range keys {
				// we do not need to report `operator` version.
				if op == "operator" {
					continue
				}
				ver := undone[op]
				if len(ver) == 1 {
					reports = append(reports, fmt.Sprintf("missing version information for %s", op))
					continue
				}
				reports = append(reports, fmt.Sprintf("upgrading %s from %s to %s", op, ver[1], ver[0]))
			}
			message := fmt.Sprintf("Cluster operator %s is still updating", actual.Name)
			if len(reports) > 0 {
				message = fmt.Sprintf("Cluster operator %s is still updating: %s", actual.Name, strings.Join(reports, ", "))
			}
			lastErr = &payload.UpdateError{
				Nested:  errors.New(lowerFirst(message)),
				Reason:  "ClusterOperatorNotAvailable",
				Message: message,
				Name:    actual.Name,
			}
			return false, nil
		}

		available := false
		progressing := true
		failing := true
		var failingCondition *configv1.ClusterOperatorStatusCondition
		degradedValue := true
		var degradedCondition *configv1.ClusterOperatorStatusCondition
		for i := range actual.Status.Conditions {
			condition := &actual.Status.Conditions[i]
			switch {
			case condition.Type == configv1.OperatorAvailable && condition.Status == configv1.ConditionTrue:
				available = true
			case condition.Type == configv1.OperatorProgressing && condition.Status == configv1.ConditionFalse:
				progressing = false
			case condition.Type == configv1.OperatorFailing:
				if condition.Status == configv1.ConditionFalse {
					failing = false
				}
				failingCondition = condition
			case condition.Type == configv1.OperatorDegraded:
				if condition.Status == configv1.ConditionFalse {
					degradedValue = false
				}
				degradedCondition = condition
			}
		}

		// If degraded was an explicitly set condition, use that. If not, use the deprecated failing.
		degraded := failing
		if degradedCondition != nil {
			degraded = degradedValue
		}

		switch mode {
		case resourcebuilder.InitializingMode:
			// during initialization we allow degraded as long as the component goes available
			if available && (!progressing || len(expected.Status.Versions) > 0) {
				return true, nil
			}
		default:
			// if we're at the correct version, and available, and not degraded, we are done
			// if we're available, not degraded, and not progressing, we're also done
			// TODO: remove progressing once all cluster operators report expected versions
			if available && (!progressing || len(expected.Status.Versions) > 0) && !degraded {
				return true, nil
			}
		}

		condition := failingCondition
		if degradedCondition != nil {
			condition = degradedCondition
		}
		if condition != nil && condition.Status == configv1.ConditionTrue {
			message := fmt.Sprintf("Cluster operator %s is reporting a failure", actual.Name)
			if len(condition.Message) > 0 {
				message = fmt.Sprintf("Cluster operator %s is reporting a failure: %s", actual.Name, condition.Message)
			}
			lastErr = &payload.UpdateError{
				Nested:  errors.New(lowerFirst(message)),
				Reason:  "ClusterOperatorDegraded",
				Message: message,
				Name:    actual.Name,
			}
			return false, nil
		}

		lastErr = &payload.UpdateError{
			Nested: fmt.Errorf("cluster operator %s is not done; it is available=%v, progressing=%v, degraded=%v",
				actual.Name, available, progressing, degraded,
			),
			Reason:  "ClusterOperatorNotAvailable",
			Message: fmt.Sprintf("Cluster operator %s has not yet reported success", actual.Name),
			Name:    actual.Name,
		}
		return false, nil
	}, ctx.Done())

	// how long we waited
	duration := time.Now().Sub(startTime)

	if err != nil {
		if err == wait.ErrWaitTimeout && lastErr != nil {
			eventRecorder.Eventf(involvedObjectRef, corev1.EventTypeWarning, "ClusterOperatorWaitFailed", "error waiting for clusteroperator/%s after %v: %v", expected.Name, duration, lastErr)
			return lastErr
		}
		eventRecorder.Eventf(involvedObjectRef, corev1.EventTypeWarning, "ClusterOperatorWaitFailed", "error waiting for clusteroperator/%s after %v: %v", expected.Name, duration, lastErr)
		return err
	}

	eventRecorder.Eventf(involvedObjectRef, corev1.EventTypeNormal, "ClusterOperatorWaitSucceeded", "finished waiting for clusteroperator/%s after %v", expected.Name, duration)
	return nil
}

func lowerFirst(str string) string {
	for i, v := range str {
		return string(unicode.ToLower(v)) + str[i+1:]
	}
	return ""
}
