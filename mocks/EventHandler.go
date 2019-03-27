// Code generated by mockery v1.0.0. DO NOT EDIT.

package mocks

import mock "github.com/stretchr/testify/mock"

import wire "gitlab.dusk.network/dusk-core/dusk-go/pkg/p2p/wire"

// EventHandler is an autogenerated mock type for the EventHandler type
type EventHandler struct {
	mock.Mock
}

// NewEvent provides a mock function with given fields:
func (_m *EventHandler) NewEvent() wire.Event {
	ret := _m.Called()

	var r0 wire.Event
	if rf, ok := ret.Get(0).(func() wire.Event); ok {
		r0 = rf()
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(wire.Event)
		}
	}

	return r0
}

// Priority provides a mock function with given fields: _a0, _a1
func (_m *EventHandler) Priority(_a0 wire.Event, _a1 wire.Event) wire.Event {
	ret := _m.Called(_a0, _a1)

	var r0 wire.Event
	if rf, ok := ret.Get(0).(func(wire.Event, wire.Event) wire.Event); ok {
		r0 = rf(_a0, _a1)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(wire.Event)
		}
	}

	return r0
}

// Stage provides a mock function with given fields: _a0
func (_m *EventHandler) Stage(_a0 wire.Event) (uint64, uint8) {
	ret := _m.Called(_a0)

	var r0 uint64
	if rf, ok := ret.Get(0).(func(wire.Event) uint64); ok {
		r0 = rf(_a0)
	} else {
		r0 = ret.Get(0).(uint64)
	}

	var r1 uint8
	if rf, ok := ret.Get(1).(func(wire.Event) uint8); ok {
		r1 = rf(_a0)
	} else {
		r1 = ret.Get(1).(uint8)
	}

	return r0, r1
}

// Verify provides a mock function with given fields: _a0
func (_m *EventHandler) Verify(_a0 wire.Event) error {
	ret := _m.Called(_a0)

	var r0 error
	if rf, ok := ret.Get(0).(func(wire.Event) error); ok {
		r0 = rf(_a0)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}