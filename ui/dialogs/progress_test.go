package dialogs

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("progress bar dialog", Ordered, func() {
	var progressDialog *ProgressDialog

	BeforeAll(func() {
		progressDialog = NewProgressDialog()
	})

	It("display", func() {
		progressDialog.Display()
		Expect(progressDialog.IsDisplay()).To(Equal(true))
	})

	It("set rect", func() {
		x := 0
		y := 0
		width := 50
		height := 20
		// wants
		wWants := prgMinWidth
		hWants := 3 // progress bar has fixed height 3 rows
		spaceWidth := (width - wWants) / 2
		spaceHeight := (height - hWants) / 2
		xWants := x + spaceWidth
		yWants := y + spaceHeight

		// set rects
		progressDialog.SetRect(x, y, width, height)
		// get rects
		x1, y1, w1, h1 := progressDialog.Box.GetRect()
		Expect(x1).To(Equal(xWants))
		Expect(y1).To(Equal(yWants))
		Expect(w1).To(Equal(wWants))
		Expect(h1).To(Equal(hWants))
	})

	It("hide", func() {
		progressDialog.Hide()
		Expect(progressDialog.IsDisplay()).To(Equal(false))
	})

})
