Files that the rc.8 installer (tag v0.1.0-rc.8, commit 6608580c3192d8b1ed8b73ada005a313c5486a84)
left in a temporary home that held the user's own settings (goldenUserFiles in
install_golden_harness_test.go), after "install" with the binary "/opt/zalki tools/zalki".
The home path is written as <HOME>. GENERATOR.go.txt is the test that made them; it was run
inside a copy of the rc.8 tree, together with install_golden_harness_test.go.
The tests read these files as an installation made by an earlier release.
