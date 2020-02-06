package auth

import (
	"context"
	"sync"
	"testing"
	"time"

	configv1alpha1 "github.com/kiosk-sh/kiosk/pkg/apis/config/v1alpha1"
	"github.com/kiosk-sh/kiosk/pkg/util"
	testingutil "github.com/kiosk-sh/kiosk/pkg/util/testing"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/authentication/user"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

type cacheGetTest struct {
	in       []string
	expected []string

	objs     []runtime.Object
	retrieve func(client.Client, []string) ([]string, error)
}

func getNamespaces(client client.Client, namespaces []string) ([]string, error) {
	objs, err := GetNamespaces(context.TODO(), client, namespaces)
	if err != nil {
		return nil, err
	}

	strings := []string{}
	for _, obj := range objs {
		strings = append(strings, obj.Name)
	}

	return strings, nil
}

func getAccounts(client client.Client, accounts []string) ([]string, error) {
	objs, err := GetAccounts(context.TODO(), client, accounts)
	if err != nil {
		return nil, err
	}

	strings := []string{}
	for _, obj := range objs {
		strings = append(strings, obj.Name)
	}

	return strings, nil
}

func TestRetrieveFromCache(t *testing.T) {
	tests := map[string]*cacheGetTest{
		"No namespaces found": &cacheGetTest{
			in:       []string{"*"},
			expected: []string{},
			retrieve: getNamespaces,
		},
		"No accounts found": &cacheGetTest{
			in:       []string{"*"},
			expected: []string{},
			retrieve: getAccounts,
		},
		"Get single namespaces": &cacheGetTest{
			in:       []string{"test", "test2"},
			expected: []string{"test"},
			objs: []runtime.Object{
				&corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test",
					},
				},
			},
			retrieve: getNamespaces,
		},
		"Get single accounts": &cacheGetTest{
			in:       []string{"test", "test2"},
			expected: []string{"test"},
			objs: []runtime.Object{
				&configv1alpha1.Account{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test",
					},
				},
			},
			retrieve: getAccounts,
		},
		"Get namespace list": &cacheGetTest{
			in:       []string{"*"},
			expected: []string{"test"},
			objs: []runtime.Object{
				&corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test",
					},
				},
			},
			retrieve: getNamespaces,
		},
		"Get account list": &cacheGetTest{
			in:       []string{"*"},
			expected: []string{"test"},
			objs: []runtime.Object{
				&configv1alpha1.Account{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test",
					},
				},
			},
			retrieve: getAccounts,
		},
	}

	scheme := testingutil.NewScheme()
	for testName, test := range tests {
		client := testingutil.NewFakeClient(scheme)
		for _, obj := range test.objs {
			err := client.Create(context.TODO(), obj)
			if err != nil {
				t.Fatal(err)
			}
		}

		real, err := test.retrieve(client, test.in)
		if err != nil {
			t.Fatal(err)
		}

		if util.StringsEqual(real, test.expected) == false {
			t.Fatalf("Test %s: expected %#+v but got %#+v", testName, test.expected, real)
		}
	}
}

func TestCache(t *testing.T) {
	scheme := testingutil.NewScheme()
	client := testingutil.NewFakeClient(scheme)
	informerCache := testingutil.NewFakeCache(scheme)

	cache, err := NewAuthCache(client, informerCache, zap.New(func(o *zap.Options) {}))
	if err != nil {
		t.Fatal(err)
	}

	authcache := cache.(*authCache)
	fakeAccessor := &fakeAccessor{
		allowedNamespaces: map[string][]string{},
		allowedAccounts:   map[string][]string{},
	}
	authcache.accessor = fakeAccessor

	stopChan := make(chan struct{})
	defer close(stopChan)

	// Start the cache
	go authcache.Run(stopChan)

	// Make sure the store is empty
	if authcache.queue.Len() > 0 {
		t.Fatalf("Queue is not empty")
	}
	if len(authcache.allowedNamespaceStore.List()) > 0 || len(authcache.allowedAccountStore.List()) > 0 {
		t.Fatalf("Store is non empty")
	}

	// Update accessor
	fakeAccessor.lock.Lock()
	fakeAccessor.allowedNamespaces["user:foo"] = []string{"test", "test2"}
	fakeAccessor.lock.Unlock()

	authcache.roleBindingInformer.(*testingutil.FakeInformer).Add(&rbacv1.RoleBinding{
		Subjects: []rbacv1.Subject{
			rbacv1.Subject{
				Kind: "User",
				Name: "foo",
			},
		},
	})

	// Wait for cache
	err = wait.Poll(time.Millisecond*10, time.Second*5, func() (bool, error) {
		_, ok := authcache.allowedNamespaceStore.Get("user:foo")
		return ok, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Update accessor
	fakeAccessor.lock.Lock()
	fakeAccessor.allowedAccounts["group:bar"] = []string{"foo", "bar"}
	fakeAccessor.lock.Unlock()

	authcache.accountInformer.(*testingutil.FakeInformer).Add(&configv1alpha1.Account{
		Spec: configv1alpha1.AccountSpec{
			Subjects: []rbacv1.Subject{
				rbacv1.Subject{
					Kind: "Group",
					Name: "bar",
				},
			},
		},
	})

	// Wait for cache
	err = wait.Poll(time.Millisecond*10, time.Second*5, func() (bool, error) {
		_, ok := authcache.allowedAccountStore.Get("group:bar")
		return ok, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Check if we get the correct results
	user := &user.DefaultInfo{
		Name:   "foo",
		Groups: []string{"bar"},
	}

	verbs := []string{"get", "list", "watch", "create", "update", "delete", "bad"}

	for _, verb := range verbs {
		accounts, err := authcache.GetAccountsForUser(user, verb)
		if verb != "bad" && err != nil {
			t.Fatal(err)
		} else if verb == "bad" {
			if err == nil {
				t.Fatalf("Expected error for verb 'bad'")
			}

			continue
		}

		if !util.StringsEqual(accounts, fakeAccessor.allowedAccounts["group:bar"]) {
			t.Fatalf("Expected accounts %#+v, got %#+v", fakeAccessor.allowedAccounts["group:bar"], accounts)
		}

		namespaces, err := authcache.GetNamespacesForUser(user, verb)
		if err != nil {
			t.Fatal(err)
		}

		if !util.StringsEqual(namespaces, fakeAccessor.allowedNamespaces["user:foo"]) {
			t.Fatalf("Expected namespace %#+v, got %#+v", fakeAccessor.allowedNamespaces["user:foo"], namespaces)
		}
	}
}

type fakeAccessor struct {
	lock sync.Mutex

	allowedNamespaces map[string][]string
	allowedAccounts   map[string][]string
}

func (f *fakeAccessor) RetrieveAllowedNamespaces(ctx context.Context, subject, verb string) ([]string, error) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.allowedNamespaces == nil {
		return nil, nil
	}

	return f.allowedNamespaces[subject], nil
}

func (f *fakeAccessor) RetrieveAllowedAccounts(ctx context.Context, subject, verb string) ([]string, error) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.allowedAccounts == nil {
		return nil, nil
	}

	return f.allowedAccounts[subject], nil
}
