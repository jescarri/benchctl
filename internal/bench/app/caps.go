package app

// Capabilities is a static, model-level capability sheet taken from the
// manufacturer's datasheet (see Source), plus behaviour verified on hardware
// (Notes). An agent uses it to pick settings without trial and error; the
// instrument still has the final word, because drivers read back every set.
type Capabilities struct {
	Model    string         `json:"model"`
	Role     string         `json:"role"`
	Channels int            `json:"channels"`
	Source   string         `json:"source"`
	Specs    map[string]any `json:"specs"`
	Commands []string       `json:"commands"`
	Notes    []string       `json:"notes,omitempty"`
}

var capSheets = map[string]Capabilities{
	"SDS1104X-E": {
		Model: "SDS1104X-E", Role: "scope", Channels: 4,
		Source: "Siglent SDS1000X-E Data Sheet Rev. 04D (Aug 2021), " +
			"https://siglentna.com/wp-content/uploads/dlm_uploads/2021/08/SDS1000X-E_DataSheet_EN04D.pdf",
		Specs: map[string]any{
			"bandwidth_hz":     100e6,
			"rise_time_s":      3.5e-9,
			"bw_limit_hz":      20e6,
			"adc_bits":         8,
			"adcs":             "2 × 1 GSa/s; C1+C2 share one, C3+C4 the other",
			"sample_rate":      map[string]float64{"one_channel_per_pair": 1e9, "both_channels_of_pair": 500e6},
			"memory_depth_pts": map[string]float64{"one_channel_per_pair": 14e6, "both_channels_of_pair": 7e6},
			"vdiv_range_v":     []float64{500e-6, 10},
			"vdiv_steps":       "1-2-5 sequence (probe 1X)",
			"offset_range_v": []map[string]any{
				{"vdiv": "500 µV–118 mV", "offset": 2.0},
				{"vdiv": "120 mV–1.18 V", "offset": 20.0},
				{"vdiv": "1.2 V–10 V", "offset": 200.0},
			},
			"input":               "1 MΩ ±2% || 15 pF ±2 pF",
			"max_input_v":         "≤400 Vpk (DC + peak AC ≤10 kHz), 1 MΩ",
			"couplings":           []string{"dc", "ac", "gnd"},
			"probe_attenuations":  []float64{0.1, 0.2, 0.5, 1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 2000, 5000, 10000},
			"dc_gain_accuracy":    "≤±3.0% (5 mV/div–10 V/div), ≤±4.0% (≤2 mV/div)",
			"tdiv_range_s":        []float64{1e-9, 100},
			"roll_mode_tdiv_s":    []float64{50e-3, 100},
			"horizontal_divs":     14,
			"vertical_divs":       8,
			"timebase_accuracy":   "±25 ppm",
			"capture_rate_wfm_s":  map[string]float64{"normal": 100000, "sequence": 400000},
			"trigger_types":       []string{"edge", "slope", "pulse", "window", "runt", "interval", "dropout", "pattern", "video"},
			"trigger_modes":       []string{"auto", "normal", "single"},
			"trigger_sources":     []string{"C1", "C2", "C3", "C4", "LINE"},
			"trigger_level_range": "±4.5 div from screen centre",
			"trigger_holdoff_s":   []float64{80e-9, 1.5},
			"trigger_delay":       "pre-trigger 0–100% of memory; delay 0–10,000 div",
			"measurement_count":   38,
			"math":                []string{"+", "-", "*", "/", "FFT", "d/dt", "∫dt", "√"},
			"fft_windows":         []string{"rectangular", "blackman", "hanning", "hamming", "flattop"},
			"display":             "7-inch TFT-LCD 800×480",
			"screenshot":          "SCDP → 800×480 16-bit BMP (verified)",
			"io":                  []string{"USB Host", "USB Device (USBTMC)", "LAN (VXI-11)", "Pass/Fail", "Trigger Out"},
			"benchctl_measurements": []string{"pkpk", "max", "min", "ampl", "top", "base", "cmean", "mean", "stdev", "vstd", "rms", "crms",
				"ovsn", "fpre", "ovsp", "rpre", "levelx", "per", "freq", "pwid", "nwid", "rise", "fall", "wid", "duty", "nduty", "delay", "timel"},
		},
		Commands: []string{"scope setup", "scope timebase", "scope trigger", "scope autoset",
			"scope measure", "scope capture", "scope screenshot", "screenshot", "raw", "script"},
		Notes: []string{
			"Legacy Siglent command set (C1:VDIV, TDIV, PAVA, WF?); no SYST:ERR?: errors come from *ESR?, CMR?, EXR?.",
			"Out-of-range values are clamped silently; benchctl reads back every setting and fails on a mismatch.",
			"PAVA returns **** for invalid measurements (reported as null). PKPK reads ~6% high from noise at 200 mV/div; prefer AMPL/RMS.",
			"*OPC does not set ESR bit 0 (no event status): completion uses *OPC?. *OPC? returns immediately after ASET.",
			"Autoset takes about one screen (14 × TDIV) plus ~2 s; benchctl waits until settings change and settle.",
			"Every WFSU form and TRSE with a hold-off argument flag a spurious CME; benchctl verifies those by read-back.",
			"SANU? only answers for C1; capture takes the point count from WAVEDESC.",
			"WAVEDESC horiz_offset = -TRDL; benchctl's time axis puts the trigger at t = 0 (verified with ±delays).",
			"VXI-11: device_lock accepted but not enforced; device_readstb always 0; interrupt channel (SRQ) never connects.",
			"Raw socket (5025) never closes sessions; repeated raw connections wedge it until a power cycle. Use VXI-11.",
		},
	},
	"SDG1032X": {
		Model: "SDG1032X", Role: "generator", Channels: 2,
		Source: "Siglent SDG1000X Data Sheet EN01I (2025), " +
			"https://siglentna.com/wp-content/uploads/dlm_uploads/2025/04/SDG1000X_DataSheet_EN01I.pdf",
		Specs: map[string]any{
			"bandwidth_hz":         30e6,
			"sample_rate":          150e6,
			"vertical_bits":        14,
			"frequency_resolution": 1e-6,
			"frequency_accuracy":   "±25 ppm (1st year, 0–40 °C)",
			"sine_hz":              []float64{1e-6, 30e6},
			"square_hz":            []float64{1e-6, 30e6},
			"square_rise_fall_s":   4.2e-9,
			"square_duty_pct":      []float64{0.001, 99.999},
			"pulse_hz":             []float64{1e-6, 12.5e6},
			"pulse_width_min_s":    32.6e-9,
			"pulse_edge_s":         []float64{16.8e-9, 22.4},
			"ramp_hz":              []float64{1e-6, 500e3},
			"ramp_symmetry_pct":    []float64{0, 100},
			"noise_bandwidth_hz":   60e6,
			"arb_length_pts":       map[string]any{"dds": 16384, "truearb": []float64{2, 16384}},
			"arb_builtin_waves":    196,
			"harmonics":            "up to 16th order; even, odd or all",
			"amplitude_hiz_vpp":    map[string][]float64{"≤10 MHz": {2e-3, 20}, ">10 MHz": {2e-3, 10}},
			"amplitude_50ohm_vpp":  map[string][]float64{"≤10 MHz": {1e-3, 10}, ">10 MHz": {1e-3, 5}},
			"amplitude_accuracy":   "±(1% + 1 mVpp), 10 kHz sine, 0 V offset",
			"dc_range_v":           map[string][]float64{"hiz": {-10, 10}, "50ohm": {-5, 5}},
			"dc_accuracy":          "±(1% + 3 mV), HiZ",
			"output_impedance":     "50 Ω ±0.5 Ω",
			"output_current_ma":    200,
			"crosstalk":            "-60 dBc",
			"loads":                []string{"hiz", "50..100000 Ω"},
			"waves":                []string{"sine", "square", "ramp", "pulse", "noise", "dc", "arb"},
			"modulation":           []string{"AM", "DSB-AM", "FM", "PM", "FSK", "ASK", "PSK", "PWM"},
			"sweep":                "linear/log, up/down, 1 ms–500 s, source internal/external/manual",
			"burst":                "N-cycle (1–1,000,000), infinite, gated; trigger internal/external/manual",
			"counter":              "0.1 Hz–200 MHz (DC coupled), 10 Hz–200 MHz (AC); freq, period, ±width, duty",
			"reference_clock":      "10 MHz in (≥1.4 Vpp) / out",
			"display":              "4.3-inch 480×272",
			"screenshot":           "SCDP → 480×272 24-bit BMP (verified)",
			"io":                   []string{"USB Host", "USB Device (USBTMC)", "LAN (VXI-11)"},
		},
		Commands: []string{"fg set", "fg output", "fg state", "screenshot", "raw", "script"},
		Notes: []string{
			"AMP is Vpp into the configured load; with load=hiz into a 1 MΩ scope input, set = displayed.",
			"Out-of-range values are clamped silently (e.g. FRQ 1E12 → 30 MHz); benchctl reads back every setting.",
			"Full IEEE 488.2 status model: *OPC sets ESR bit 0, so `raw --wait poll` works.",
			"VXI-11: device_lock accepted but not enforced; device_readstb always 0; interrupt channel (SRQ) never connects.",
			"Modulation (MDWV), sweep (SWWV), burst (BTWV) and arbitrary waves (ARWV/WVDT) are reachable via raw/script.",
		},
	},
	"SDM3045X": {
		Model: "SDM3045X", Role: "dmm", Channels: 1,
		Source: "Siglent SDM3045X Data Sheet EN04A, " +
			"https://www.siglenteu.com/wp-content/uploads/dlm_uploads/2023/02/SDM3045X_DataSheet_E04A.pdf",
		Specs: map[string]any{
			"digits":              "4½ (66,000 count)",
			"max_reading_rate":    "150 readings/s",
			"dcv_ranges_v":        []float64{0.6, 6, 60, 600, 1000},
			"dcv_accuracy":        "0.06% + 8 counts (1 year)",
			"dcv_input":           "600 mV: 10 MΩ or >10 GΩ selectable; other ranges 10 MΩ ±2%",
			"acv_ranges_v":        []float64{0.6, 6, 60, 600, 750},
			"acv_bandwidth_hz":    []float64{20, 100e3},
			"dci_ranges_a":        []float64{600e-6, 6e-3, 60e-3, 600e-3, 6, 10},
			"aci_ranges_a":        []float64{60e-3, 600e-3, 6, 10},
			"aci_bandwidth_hz":    []float64{20, 10e3},
			"res_ranges_ohm":      []float64{600, 6e3, 60e3, 600e3, 6e6, 60e6, 100e6},
			"resistance_modes":    []string{"2-wire", "4-wire"},
			"cap_ranges_f":        []float64{2e-9, 20e-9, 200e-9, 2e-6, 20e-6, 200e-6, 10e-3},
			"freq_range_hz":       []float64{20, 500e3},
			"period_range_s":      []float64{2e-6, 0.05},
			"continuity":          "fixed 2 kΩ range, 1 mA",
			"diode":               "0–4 V, 1 mA",
			"temperature":         "RTD (α=0.00385) and thermocouples B, E, J, K, N, R, S, T",
			"max_input":           "1000 V DC on all voltage ranges; 10 A (12 A / 250 V fuse)",
			"samples_per_trigger": []int{1, 10000},
			"math":                []string{"min/max/average", "stddev", "dBm", "dB", "relative", "pass/fail", "hold", "histogram", "trend"},
			"display":             "4.3-inch 480×272",
			"screenshot":          "SCDP → 480×272 24-bit BMP (verified)",
			"io":                  []string{"USB Host", "USB Device (USBTMC)", "LAN (VXI-11)", "USB-GPIB (option)"},
			"functions":           []string{"vdc", "vac", "idc", "iac", "res", "fres", "freq", "per", "cap", "diode", "cont", "temp"},
		},
		Commands: []string{"dmm config", "dmm read", "screenshot", "raw", "script"},
		Notes: []string{
			"SCPI-1999 tree; SYST:ERR? is drained after every set.",
			"Full IEEE 488.2 status model: *OPC sets ESR bit 0, so `raw --wait poll` works.",
			"VXI-11: device_lock accepted but not enforced; device_readstb always 0; interrupt channel (SRQ) never connects.",
		},
	},
}

func capsFor(model string) (Capabilities, bool) {
	c, ok := capSheets[model]
	return c, ok
}
