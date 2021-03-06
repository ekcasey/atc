package auth_test

import (
	"encoding/json"

	"code.cloudfoundry.org/lager/lagertest"
	"github.com/concourse/atc/auth"
	"github.com/concourse/atc/auth/genericoauth"
	"github.com/concourse/atc/auth/github"
	"github.com/concourse/atc/auth/routes"
	"github.com/concourse/atc/auth/uaa"
	"github.com/concourse/atc/dbng/dbngfakes"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("OAuthFactory", func() {
	var (
		oauthFactory auth.OAuthFactory
		fakeTeam     *dbngfakes.FakeTeam
		authConfig   map[string]*json.RawMessage
	)

	BeforeEach(func() {
		fakeTeam = new(dbngfakes.FakeTeam)

		oauthFactory = auth.NewOAuthFactory(
			lagertest.NewTestLogger("test"),
			"http://foo.bar",
			routes.OAuthRoutes,
			routes.OAuthCallback,
		)
	})

	Describe("GetProvider", func() {
		Context("when asking for github provider", func() {
			Context("when github provider is setup", func() {
				It("returns back GitHub's auth provider", func() {
					data := []byte(`
					{
						"ClientID": "user1",
						"ClientSecret": "password1",
						"Users": ["thecandyman"]
					}`)
					authConfig = map[string]*json.RawMessage{
						"github": (*json.RawMessage)(&data),
					}

					fakeTeam.NameReturns("some-team")
					fakeTeam.AuthReturns(authConfig)

					provider, found, err := oauthFactory.GetProvider(fakeTeam, github.ProviderName)
					Expect(err).NotTo(HaveOccurred())
					Expect(found).To(BeTrue())
					Expect(provider).NotTo(BeNil())
				})
			})

			Context("when github provider is not setup", func() {
				It("returns false", func() {
					fakeTeam.NameReturns("some-team")
					_, found, err := oauthFactory.GetProvider(fakeTeam, github.ProviderName)
					Expect(err).NotTo(HaveOccurred())
					Expect(found).To(BeFalse())
				})
			})
		})

		Context("when asking for uaa provider", func() {
			Context("when UAA provider is setup", func() {
				It("returns back UAA's auth provider", func() {
					data := []byte(`
					{
						"ClientID": "user1",
						"ClientSecret": "password1"
					}`)
					authConfig = map[string]*json.RawMessage{
						"uaa": (*json.RawMessage)(&data),
					}

					fakeTeam.NameReturns("some-team")
					fakeTeam.AuthReturns(authConfig)

					provider, found, err := oauthFactory.GetProvider(fakeTeam, uaa.ProviderName)
					Expect(err).NotTo(HaveOccurred())
					Expect(found).To(BeTrue())
					Expect(provider).NotTo(BeNil())
				})
			})

			Context("when uaa provider is not setup", func() {
				It("returns false", func() {
					fakeTeam.NameReturns("some-team")
					_, found, err := oauthFactory.GetProvider(fakeTeam, uaa.ProviderName)
					Expect(err).NotTo(HaveOccurred())
					Expect(found).To(BeFalse())
				})
			})
		})

		Context("when asking for generic oauth", func() {
			Context("when Generic OAuth provider is setup", func() {
				It("returns back GOA's auth provider", func() {
					data := []byte(`
					{
						"ClientID": "user1",
						"ClientSecret": "password1"
					}`)
					authConfig = map[string]*json.RawMessage{
						"oauth": (*json.RawMessage)(&data),
					}

					fakeTeam.NameReturns("some-team")
					fakeTeam.AuthReturns(authConfig)

					provider, found, err := oauthFactory.GetProvider(fakeTeam, genericoauth.ProviderName)
					Expect(err).NotTo(HaveOccurred())
					Expect(found).To(BeTrue())
					Expect(provider).NotTo(BeNil())
				})

				Context("when Generic OAuth provider is not setup", func() {
					It("returns false", func() {
						fakeTeam.NameReturns("some-team")
						_, found, err := oauthFactory.GetProvider(fakeTeam, genericoauth.ProviderName)
						Expect(err).NotTo(HaveOccurred())
						Expect(found).To(BeFalse())
					})
				})
			})
		})

		Context("when asking for unknown provider", func() {
			It("returns false", func() {
				fakeTeam.NameReturns("some-team")
				_, found, err := oauthFactory.GetProvider(fakeTeam, "bogus")
				Expect(err).NotTo(HaveOccurred())
				Expect(found).To(BeFalse())
			})
		})
	})
})
